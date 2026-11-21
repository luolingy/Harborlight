package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"harborlight/internal/auth"
	"harborlight/internal/config"
	"harborlight/internal/dockerx"
	"harborlight/internal/store"
)

type Server struct {
	cfg      *config.Config
	auth     *auth.Service
	dk       *dockerx.Manager
	db       *store.DBWrapper
	tmpl     map[string]*template.Template
	funcs    template.FuncMap
	deviceMu sync.Mutex
	devices  map[string]*dockerx.Manager
}

func New(cfg *config.Config, a *auth.Service, dk *dockerx.Manager, db *store.DBWrapper) (*Server, error) {
	s := &Server{cfg: cfg, auth: a, dk: dk, db: db, devices: make(map[string]*dockerx.Manager)}
	tm, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	s.tmpl = tm
	return s, nil
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger, middleware.Recoverer, middleware.Compress(5))

	r.Get("/", s.requireAuth(s.handleHome))
	r.Get("/login", s.handleLoginPage)
	r.Post("/login/demo", s.handleDemoLogin)
	r.Get("/auth/start", s.handleAuthStart)
	r.Get("/auth/callback", s.handleAuthCallback)
	r.Post("/logout", s.requireAuth(s.handleLogout))
	r.Post("/devices", s.requireAuth(s.handleCreateDevice))
	r.Post("/devices/{id}/delete", s.requireAuth(s.handleDeleteDevice))

	r.Post("/instances", s.requireAuth(s.handleCreateInstance))
	r.Route("/instances/{id}", func(ir chi.Router) {
		ir.Get("/", s.requireAuth(s.handleInstanceDetail))
		ir.Post("/start", s.requireAuth(s.handleAction("start")))
		ir.Post("/stop", s.requireAuth(s.handleAction("stop")))
		ir.Post("/restart", s.requireAuth(s.handleAction("restart")))
		ir.Post("/delete", s.requireAuth(s.handleDelete))
	})

	r.Get("/api/status", s.requireAuth(s.handleAPIStatus))
	r.Get("/api/instances/{id}", s.requireAuth(s.handleAPIInstance))
	r.Get("/api/instances/{id}/logs", s.requireAuth(s.handleAPILogs))

	return r
}

// ---- 中间件 ----

type ctxKey string

const userKey ctxKey = "user"

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.auth.GetSession(r)
		if sess == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r.WithContext(withUser(r.Context(), sess)))
	}
}

func withUser(ctx contextLike, u *auth.Session) contextLike { return ctxWith(ctx, userKey, u) }

func (s *Server) currentUser(r *http.Request) *auth.Session {
	u, _ := r.Context().Value(userKey).(*auth.Session)
	return u
}

// ---- 登录 ----

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth.GetSession(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login", map[string]any{
		"OAuthEnabled": s.auth.OAuthEnabled(),
		"DemoLogin":    s.cfg.DemoLogin,
		"Provider":     strings.Title(s.cfg.Provider),
		"Error":        r.URL.Query().Get("error"),
	})
}

func (s *Server) handleAuthStart(w http.ResponseWriter, r *http.Request) {
	if !s.auth.OAuthEnabled() {
		http.Redirect(w, r, "/login?error=oauth+not+configured", http.StatusSeeOther)
		return
	}
	state, err := s.auth.SaveState(r)
	if err != nil {
		http.Redirect(w, r, "/login?error=session+store+failure", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.auth.AuthCodeURL(state), http.StatusSeeOther)
}

func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Redirect(w, r, "/login?error="+e, http.StatusSeeOther)
		return
	}
	if !s.auth.CheckState(r, q.Get("state")) {
		http.Redirect(w, r, "/login?error=invalid+state", http.StatusSeeOther)
		return
	}
	ou, err := s.auth.Exchange(r.Context(), q.Get("code"))
	if err != nil {
		http.Redirect(w, r, "/login?error="+urlEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if s.cfg.IsAdminEmail(ou.Email) {
		ou.Role = "admin"
	}
	u, err := s.db.UpsertUser(r.Context(), ou)
	if err != nil {
		http.Redirect(w, r, "/login?error=user+persist+failed", http.StatusSeeOther)
		return
	}
	if err := s.auth.CreateSession(w, r, u); err != nil {
		http.Redirect(w, r, "/login?error=session+create+failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDemoLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.DemoLogin {
		http.Redirect(w, r, "/login?error=demo+login+disabled", http.StatusSeeOther)
		return
	}
	u, err := s.db.UpsertUser(r.Context(), &store.User{
		Email: "demo@harborlight.local", Name: "演示用户", Role: "admin",
	})
	if err != nil {
		http.Redirect(w, r, "/login?error=user+persist+failed", http.StatusSeeOther)
		return
	}
	if err := s.auth.CreateSession(w, r, u); err != nil {
		http.Redirect(w, r, "/login?error=session+create+failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.DestroySession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if u := s.currentUser(r); u == nil || u.Role != "admin" {
		http.Error(w, "仅管理员可以管理 Docker 设备", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) handleCreateDevice(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectErr(w, r, "表单解析失败")
		return
	}
	device := store.Device{
		Name:     sanitizeName(r.FormValue("name")),
		Endpoint: strings.TrimSpace(r.FormValue("endpoint")),
		TLS:      r.FormValue("tls") == "on",
		CAPath:   strings.TrimSpace(r.FormValue("ca_path")),
		CertPath: strings.TrimSpace(r.FormValue("cert_path")),
		KeyPath:  strings.TrimSpace(r.FormValue("key_path")),
		DataDir:  strings.TrimSpace(r.FormValue("data_dir")),
	}
	if device.Name == "" || device.Endpoint == "" || device.DataDir == "" {
		s.redirectErr(w, r, "设备名称、地址和远端数据目录不能为空")
		return
	}
	if !device.TLS {
		s.redirectErr(w, r, "远程 Docker Engine 必须启用双向 TLS")
		return
	}
	if !strings.HasPrefix(device.DataDir, "/") {
		s.redirectErr(w, r, "远端数据目录必须是 Linux Docker 主机上的绝对路径")
		return
	}
	manager, err := dockerx.NewDevice(device)
	if err != nil {
		s.redirectErr(w, r, err.Error())
		return
	}
	defer manager.Close()
	ctx, cancel := contextWithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if err := manager.Ping(ctx); err != nil {
		s.redirectErr(w, r, "Docker Engine 连接失败: "+err.Error())
		return
	}
	if err := s.db.CreateDevice(&device); err != nil {
		s.redirectErr(w, r, "保存设备失败: "+err.Error())
		return
	}
	http.Redirect(w, r, "/?ok=设备+"+device.Name+"+已添加", http.StatusSeeOther)
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "id")
	count, err := s.db.DeviceInstanceCount(id)
	if err != nil || count > 0 {
		s.redirectErr(w, r, "设备仍有关联实例，不能删除")
		return
	}
	if err := s.db.DeleteDevice(id); err != nil {
		s.redirectErr(w, r, "删除设备失败: "+err.Error())
		return
	}
	s.deviceMu.Lock()
	if manager := s.devices[id]; manager != nil {
		_ = manager.Close()
		delete(s.devices, id)
	}
	s.deviceMu.Unlock()
	http.Redirect(w, r, "/?ok=设备已删除", http.StatusSeeOther)
}

func (s *Server) managerForDevice(id string) (*dockerx.Manager, error) {
	if id == "" {
		return s.dk, nil
	}
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	if manager := s.devices[id]; manager != nil {
		return manager, nil
	}
	device, err := s.db.GetDevice(id)
	if err != nil {
		return nil, err
	}
	manager, err := dockerx.NewDevice(*device)
	if err != nil {
		return nil, err
	}
	s.devices[id] = manager
	return manager, nil
}

func (s *Server) managerForInstance(inst *store.Instance) (*dockerx.Manager, error) {
	return s.managerForDevice(inst.DeviceID)
}

// ---- 仪表盘 ----

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	insts, err := s.db.ListInstances(user.UserID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	devices, err := s.db.ListDevices()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	deviceNames := make(map[string]string, len(devices))
	for _, device := range devices {
		deviceNames[device.ID] = device.Name
	}
	var wg sync.WaitGroup
	for i := range insts {
		if insts[i].ContainerID == "" {
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			manager, err := s.managerForInstance(&insts[idx])
			if err != nil {
				insts[idx].Status = "unknown"
				return
			}
			ctx, cancel := contextWithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			st := manager.State(ctx, insts[idx].ContainerID)
			_ = s.db.UpdateStatus(insts[idx].ID, st)
			insts[idx].Status = st
		}(i)
	}
	wg.Wait()
	for i := range insts {
		if insts[i].DeviceID != "" {
			insts[i].DeviceName = deviceNames[insts[i].DeviceID]
		}
	}
	dockerOK := s.dk.Ping(r.Context()) == nil
	for _, device := range devices {
		if manager, err := s.managerForDevice(device.ID); err == nil && manager.Ping(r.Context()) == nil {
			dockerOK = true
			break
		}
	}
	running := 0
	for _, in := range insts {
		if in.Status == "running" {
			running++
		}
	}
	s.render(w, "dashboard", map[string]any{
		"User":       user,
		"Instances":  insts,
		"Devices":    devices,
		"IsAdmin":    user.Role == "admin",
		"DockerOK":   dockerOK,
		"DBBackend":  s.cfg.DBSummary(),
		"RedisOn":    s.cfg.RedisURL != "",
		"Running":    running,
		"Total":      len(insts),
		"Types":      dockerx.SupportedTypes(),
		"FlashError": r.URL.Query().Get("error"),
		"FlashOK":    r.URL.Query().Get("ok"),
	})
}

func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	if err := r.ParseForm(); err != nil {
		s.redirectErr(w, r, "表单解析失败")
		return
	}
	name := sanitizeName(r.FormValue("name"))
	typ := r.FormValue("type")
	image := strings.TrimSpace(r.FormValue("image"))
	innerPort, _ := strconv.Atoi(r.FormValue("port"))

	if name == "" {
		s.redirectErr(w, r, "实例名称不能为空")
		return
	}
	if !contains(dockerx.SupportedTypes(), typ) {
		s.redirectErr(w, r, "不支持的实例类型")
		return
	}

	deviceID := strings.TrimSpace(r.FormValue("device_id"))
	manager, err := s.managerForDevice(deviceID)
	if err != nil {
		s.redirectErr(w, r, "目标设备不可用: "+err.Error())
		return
	}
	inst := &store.Instance{
		ID:        store.NewID(),
		UserID:    user.UserID,
		DeviceID:  deviceID,
		Name:      name,
		Type:      typ,
		Image:     image,
		InnerPort: innerPort,
		Status:    "creating",
	}
	ctx, cancel := contextWithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	if err := manager.Create(ctx, inst, innerPort); err != nil {
		s.redirectErr(w, r, err.Error())
		return
	}
	if err := s.db.CreateInstance(inst); err != nil {
		s.redirectErr(w, r, "实例已创建但落库失败: "+err.Error())
		return
	}
	http.Redirect(w, r, "/?ok=实例+"+name+"+创建成功", http.StatusSeeOther)
}

func (s *Server) ownedInstance(r *http.Request) (*store.Instance, bool) {
	id := chi.URLParam(r, "id")
	user := s.currentUser(r)
	inst, err := s.db.GetInstance(id)
	if err != nil || inst.UserID != user.UserID && user.Role != "admin" {
		return nil, false
	}
	return inst, true
}

func (s *Server) handleAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, ok := s.ownedInstance(r)
		if !ok || inst.ContainerID == "" {
			s.redirectErr(w, r, "实例不存在或无权操作")
			return
		}
		manager, err := s.managerForInstance(inst)
		if err != nil {
			s.redirectErr(w, r, "连接设备失败: "+err.Error())
			return
		}
		ctx, cancel := contextWithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		var errAction error
		switch action {
		case "start":
			errAction = manager.Start(ctx, inst.ContainerID)
		case "stop":
			errAction = manager.Stop(ctx, inst.ContainerID)
		case "restart":
			errAction = manager.Restart(ctx, inst.ContainerID)
		}
		if errAction != nil {
			s.redirectErr(w, r, action+" 失败: "+errAction.Error())
			return
		}
		_ = s.db.UpdateStatus(inst.ID, action)
		http.Redirect(w, r, "/instances/"+inst.ID+"?ok="+action+"+成功", http.StatusSeeOther)
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.ownedInstance(r)
	if !ok {
		s.redirectErr(w, r, "实例不存在或无权操作")
		return
	}
	if inst.ContainerID != "" {
		manager, err := s.managerForInstance(inst)
		if err != nil {
			s.redirectErr(w, r, "连接设备失败: "+err.Error())
			return
		}
		ctx, cancel := contextWithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		if err := manager.Remove(ctx, inst.ContainerID); err != nil {
			s.redirectErr(w, r, "删除容器失败: "+err.Error())
			return
		}
	}
	if err := s.db.DeleteInstance(inst.ID); err != nil {
		s.redirectErr(w, r, err.Error())
		return
	}
	http.Redirect(w, r, "/?ok=实例已删除", http.StatusSeeOther)
}

func (s *Server) handleInstanceDetail(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.ownedInstance(r)
	if !ok {
		http.Error(w, "实例不存在或无权访问", 404)
		return
	}
	if inst.ContainerID != "" {
		manager, err := s.managerForInstance(inst)
		if err != nil {
			http.Error(w, "连接设备失败: "+err.Error(), 502)
			return
		}
		ctx, cancel := contextWithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		st := manager.State(ctx, inst.ContainerID)
		if st != "unknown" {
			_ = s.db.UpdateStatus(inst.ID, st)
			inst.Status = st
		}
	}
	if inst.DeviceID != "" {
		if device, err := s.db.GetDevice(inst.DeviceID); err == nil {
			inst.DeviceName = device.Name
		}
	}
	var env map[string]string
	_ = json.Unmarshal([]byte(inst.EnvJSON), &env)
	s.render(w, "instance", map[string]any{
		"User":       s.currentUser(r),
		"Instance":   inst,
		"Env":        env,
		"FlashError": r.URL.Query().Get("error"),
		"FlashOK":    r.URL.Query().Get("ok"),
	})
}

// ---- JSON API ----

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"docker": s.dk.Ping(r.Context()) == nil,
		"time":   time.Now().Format(time.RFC3339),
	})
}

func (s *Server) handleAPIInstance(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.ownedInstance(r)
	if !ok {
		writeJSON(w, map[string]any{"error": "not found"}, 404)
		return
	}
	if inst.ContainerID != "" {
		manager, err := s.managerForInstance(inst)
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()}, 502)
			return
		}
		ctx, cancel := contextWithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		inst.Status = manager.State(ctx, inst.ContainerID)
	}
	writeJSON(w, map[string]any{"status": inst.Status})
}

func (s *Server) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.ownedInstance(r)
	if !ok || inst.ContainerID == "" {
		writeJSON(w, map[string]any{"error": "not found"}, 404)
		return
	}
	manager, err := s.managerForInstance(inst)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()}, 502)
		return
	}
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if tail <= 0 || tail > 2000 {
		tail = 200
	}
	ctx, cancel := contextWithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	logs, err := manager.Logs(ctx, inst.ContainerID, tail)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()}, 500)
		return
	}
	writeJSON(w, map[string]any{"logs": logs})
}

// ---- helpers ----

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	t, ok := s.tmpl[name]
	if !ok {
		http.Error(w, "template "+name+" not found", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) redirectErr(w http.ResponseWriter, r *http.Request, msg string) {
	back := r.Header.Get("Referer")
	if back == "" {
		back = "/"
	}
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	http.Redirect(w, r, back+sep+"error="+urlEscape(msg), http.StatusSeeOther)
}

func urlEscape(s string) string { return strings.ReplaceAll(queryEscape(s), "+", "%20") }

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r > 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any, code ...int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if len(code) > 0 {
		w.WriteHeader(code[0])
	}
	_ = json.NewEncoder(w).Encode(v)
}

var _ = fmt.Sprintf // keep fmt

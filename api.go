package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

type apiServer struct {
	mgr   *Manager
	token string
}

func newAPIServer(mgr *Manager, token string) *apiServer {
	return &apiServer{mgr: mgr, token: token}
}

// apiMessages — короткий словарь сообщений, которые сервер сам формирует
// (в отличие от ошибок vCenter/govmomi, которые уже приходят на английском
// и передаются клиенту как есть). Язык выбирается по заголовку X-Lang,
// который фронтенд выставляет в соответствии с текущим языком интерфейса.
//
// apiMessages — a short dictionary for messages the server composes itself
// (as opposed to vCenter/govmomi errors, which already arrive in English
// and are passed through to the client as-is). The language is picked from
// the X-Lang header, which the frontend sets to match the current UI language.
var apiMessages = map[string]map[string]string{
	"ru": {
		"badToken":       "неверный или отсутствующий токен доступа",
		"noConnection":   "подключение к vCenter не найдено, переподключитесь",
		"noVM":           "не указана ВМ",
		"noPool":         "не указан resource pool",
		"noDatastore":    "не указан датастор",
		"noPaths":        "не передано ни одного пути для проверки",
		"sessionExpired": "Сессия vCenter истекла (обычно после ~30 минут бездействия). Подключение закрыто — подключитесь заново.",
	},
	"en": {
		"badToken":       "invalid or missing auth token",
		"noConnection":   "vCenter connection not found, please reconnect",
		"noVM":           "VM not specified",
		"noPool":         "resource pool not specified",
		"noDatastore":    "datastore not specified",
		"noPaths":        "no paths were submitted to check",
		"sessionExpired": "The vCenter session expired (usually after ~30 minutes of inactivity). The connection was closed — please reconnect.",
	},
}

// langOf возвращает язык интерфейса, который фронтенд передаёт в X-Lang,
// или "ru" по умолчанию, если заголовок не задан или неизвестен.
//
// langOf returns the UI language the frontend sends via X-Lang, defaulting
// to "ru" when the header is absent or unrecognized.
func langOf(r *http.Request) string {
	l := r.Header.Get("X-Lang")
	if _, ok := apiMessages[l]; ok {
		return l
	}
	return "ru"
}

// msg возвращает локализованное сообщение по ключу для языка запроса.
//
// msg returns the localized message for key in the request's language.
func msg(r *http.Request, key string) string {
	return apiMessages[langOf(r)][key]
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// isSessionExpired сообщает, вызвана ли ошибка истёкшей/недействительной
// сессией vCenter, а не какой-то другой проблемой.
//
// isSessionExpired reports whether err is caused by an expired/invalid
// vCenter session rather than some other problem.
func isSessionExpired(err error) bool {
	if err == nil {
		return false
	}
	if soap.IsSoapFault(err) {
		if _, ok := soap.ToSoapFault(err).VimFault().(types.NotAuthenticated); ok {
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "NotAuthenticated") || strings.Contains(msg, "not authenticated") ||
		strings.Contains(msg, "has already been terminated") || strings.Contains(msg, "session is not authenticated")
}

// writeVCenterError отвечает клиенту на ошибку операции с vCenter. Если
// причина — истёкшая сессия, соответствующее подключение убирается из
// списка активных и в ответе передаётся признак sessionExpired, чтобы
// фронтенд сразу предложил переподключиться, а не просто показал ошибку.
//
// writeVCenterError responds to a vCenter operation error. If it was caused
// by an expired session, the connection is dropped from the active list and
// the response carries a sessionExpired flag so the frontend can offer to
// reconnect right away instead of showing a plain error.
func (s *apiServer) writeVCenterError(w http.ResponseWriter, r *http.Request, connID string, err error) {
	if isSessionExpired(err) {
		ctx, cancel := ctxWithTimeout()
		_ = s.mgr.Disconnect(ctx, connID)
		cancel()
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"error":          msg(r, "sessionExpired"),
			"sessionExpired": true,
		})
		return
	}
	writeError(w, http.StatusBadGateway, err)
}

// authMiddleware требует корректный одноразовый токен в заголовке X-Auth-Token
// на каждом запросе к /api/.
//
// authMiddleware requires a valid one-time token in the X-Auth-Token header
// on every /api/ request.
func (s *apiServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Token") != s.token {
			writeError(w, http.StatusForbidden, errString(msg(r, "badToken")))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type errString string

func (e errString) Error() string { return string(e) }

func ctxWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}

// handleConnect — Подключение к vCenter (POST /api/connect).
//
// handleConnect connects to a vCenter (POST /api/connect).
func (s *apiServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Address  string `json:"address"`
		Username string `json:"username"`
		Password string `json:"password"`
		Insecure bool   `json:"insecure"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()

	info, err := s.mgr.Connect(ctx, req.Address, req.Username, req.Password, req.Insecure)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleDisconnect — Отключение от vCenter (POST /api/disconnect).
//
// handleDisconnect disconnects from a vCenter (POST /api/disconnect).
func (s *apiServer) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()

	if err := s.mgr.Disconnect(ctx, req.ID); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleVCenters — Список текущих подключений к vCenter (GET /api/vcenters).
//
// handleVCenters lists the currently active vCenter connections (GET /api/vcenters).
func (s *apiServer) handleVCenters(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.List())
}

func (s *apiServer) connFromQuery(w http.ResponseWriter, r *http.Request) (*Connection, bool) {
	id := r.URL.Query().Get("vcenter")
	conn, ok := s.mgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errString(msg(r, "noConnection")))
		return nil, false
	}
	return conn, true
}

// handleVMs — Поиск ВМ по имени или VM ID (GET /api/vms).
//
// handleVMs searches VMs by name or VM ID (GET /api/vms).
func (s *apiServer) handleVMs(w http.ResponseWriter, r *http.Request) {
	conn, ok := s.connFromQuery(w, r)
	if !ok {
		return
	}
	query := r.URL.Query().Get("query")
	byID := r.URL.Query().Get("byId") == "true"

	ctx, cancel := ctxWithTimeout()
	defer cancel()

	vms, err := s.mgr.SearchVMs(ctx, conn, query, byID)
	if err != nil {
		s.writeVCenterError(w, r, conn.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, vms)
}

// handleDisks — Список дисков ВМ с их лимитами IOPS (GET /api/disks).
//
// handleDisks lists a VM's disks with their IOPS limits (GET /api/disks).
func (s *apiServer) handleDisks(w http.ResponseWriter, r *http.Request) {
	conn, ok := s.connFromQuery(w, r)
	if !ok {
		return
	}
	vmRef := r.URL.Query().Get("vm")
	if vmRef == "" {
		writeError(w, http.StatusBadRequest, errString(msg(r, "noVM")))
		return
	}

	ctx, cancel := ctxWithTimeout()
	defer cancel()

	disks, err := s.mgr.GetDisks(ctx, conn, vmRef)
	if err != nil {
		s.writeVCenterError(w, r, conn.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, disks)
}

// handleSetLimit — Изменение лимита IOPS одного диска (POST /api/disk-limit).
//
// handleSetLimit changes the IOPS limit of a single disk (POST /api/disk-limit).
func (s *apiServer) handleSetLimit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		VCenter string `json:"vcenter"`
		VM      string `json:"vm"`
		Key     int32  `json:"key"`
		Limit   int64  `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	conn, ok := s.mgr.Get(req.VCenter)
	if !ok {
		writeError(w, http.StatusNotFound, errString(msg(r, "noConnection")))
		return
	}

	ctx, cancel := ctxWithTimeout()
	defer cancel()

	if err := s.mgr.SetDiskLimit(ctx, conn, req.VM, req.Key, req.Limit); err != nil {
		s.writeVCenterError(w, r, conn.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleResetAll — Сброс лимитов IOPS на всех дисках одной ВМ (POST /api/reset-all).
//
// handleResetAll resets IOPS limits on all disks of one VM (POST /api/reset-all).
func (s *apiServer) handleResetAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		VCenter string `json:"vcenter"`
		VM      string `json:"vm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	conn, ok := s.mgr.Get(req.VCenter)
	if !ok {
		writeError(w, http.StatusNotFound, errString(msg(r, "noConnection")))
		return
	}

	ctx, cancel := ctxWithTimeout()
	defer cancel()

	count, err := s.mgr.ResetAllLimits(ctx, conn, req.VM)
	if err != nil {
		s.writeVCenterError(w, r, conn.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"reset": count})
}

// handleResourcePools — Поиск resource pool по имени (GET /api/resourcepools).
//
// handleResourcePools searches resource pools by name (GET /api/resourcepools).
func (s *apiServer) handleResourcePools(w http.ResponseWriter, r *http.Request) {
	conn, ok := s.connFromQuery(w, r)
	if !ok {
		return
	}
	query := r.URL.Query().Get("query")

	ctx, cancel := ctxWithTimeout()
	defer cancel()

	pools, err := s.mgr.SearchResourcePools(ctx, conn, query)
	if err != nil {
		s.writeVCenterError(w, r, conn.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, pools)
}

// handleResourcePoolVMs — Список ВМ непосредственно в resource pool (GET /api/resourcepool-vms).
//
// handleResourcePoolVMs lists VMs directly inside a resource pool (GET /api/resourcepool-vms).
func (s *apiServer) handleResourcePoolVMs(w http.ResponseWriter, r *http.Request) {
	conn, ok := s.connFromQuery(w, r)
	if !ok {
		return
	}
	pool := r.URL.Query().Get("pool")
	if pool == "" {
		writeError(w, http.StatusBadRequest, errString(msg(r, "noPool")))
		return
	}

	ctx, cancel := ctxWithTimeout()
	defer cancel()

	vms, err := s.mgr.ResourcePoolVMs(ctx, conn, pool)
	if err != nil {
		s.writeVCenterError(w, r, conn.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, vms)
}

// handleResourcePoolResetAll — Массовый сброс лимитов IOPS у всех ВМ resource pool (POST /api/resourcepool-reset-all).
//
// handleResourcePoolResetAll bulk-resets IOPS limits for all VMs in a resource pool (POST /api/resourcepool-reset-all).
func (s *apiServer) handleResourcePoolResetAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		VCenter string `json:"vcenter"`
		Pool    string `json:"pool"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	conn, ok := s.mgr.Get(req.VCenter)
	if !ok {
		writeError(w, http.StatusNotFound, errString(msg(r, "noConnection")))
		return
	}

	// Пул может содержать много ВМ — даём больше времени, чем на операцию с одной ВМ.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	result, err := s.mgr.ResetPoolLimits(ctx, conn, req.Pool)
	if err != nil {
		s.writeVCenterError(w, r, conn.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDatastores — Поиск датастора по имени (GET /api/datastores).
//
// handleDatastores searches datastores by name (GET /api/datastores).
func (s *apiServer) handleDatastores(w http.ResponseWriter, r *http.Request) {
	conn, ok := s.connFromQuery(w, r)
	if !ok {
		return
	}
	query := r.URL.Query().Get("query")

	ctx, cancel := ctxWithTimeout()
	defer cancel()

	stores, err := s.mgr.SearchDatastores(ctx, conn, query)
	if err != nil {
		s.writeVCenterError(w, r, conn.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, stores)
}

// handleDatastoreVMs — Список ВМ и их дисков на конкретном датасторе (GET /api/datastore-vms).
//
// handleDatastoreVMs lists VMs and their disks on a specific datastore (GET /api/datastore-vms).
func (s *apiServer) handleDatastoreVMs(w http.ResponseWriter, r *http.Request) {
	conn, ok := s.connFromQuery(w, r)
	if !ok {
		return
	}
	ds := r.URL.Query().Get("datastore")
	if ds == "" {
		writeError(w, http.StatusBadRequest, errString(msg(r, "noDatastore")))
		return
	}

	// У датастора может быть много ВМ — даём больше времени, чем на одиночный запрос.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	vms, err := s.mgr.DatastoreVMs(ctx, conn, ds)
	if err != nil {
		s.writeVCenterError(w, r, conn.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, vms)
}

// handleDatastoreResetAll — Сброс лимитов IOPS только у дисков на конкретном датасторе (POST /api/datastore-reset-all).
//
// handleDatastoreResetAll resets IOPS limits only for disks on a specific datastore (POST /api/datastore-reset-all).
func (s *apiServer) handleDatastoreResetAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		VCenter   string `json:"vcenter"`
		Datastore string `json:"datastore"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	conn, ok := s.mgr.Get(req.VCenter)
	if !ok {
		writeError(w, http.StatusNotFound, errString(msg(r, "noConnection")))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	result, err := s.mgr.ResetDatastoreLimits(ctx, conn, req.Datastore)
	if err != nil {
		s.writeVCenterError(w, r, conn.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleCheckPaths — Проверка списка путей .vmx/.vmdk на регистрацию за ВМ и на статус zombie (POST /api/check-paths).
//
// handleCheckPaths checks a list of .vmx/.vmdk paths against VM registration and zombie status (POST /api/check-paths).
func (s *apiServer) handleCheckPaths(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		VCenter string   `json:"vcenter"`
		Paths   []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	conn, ok := s.mgr.Get(req.VCenter)
	if !ok {
		writeError(w, http.StatusNotFound, errString(msg(r, "noConnection")))
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, errString(msg(r, "noPaths")))
		return
	}

	// Полное сканирование всех ВМ vCenter — на крупной инсталляции может занять время.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	results, err := s.mgr.CheckPaths(ctx, conn, req.Paths)
	if err != nil {
		s.writeVCenterError(w, r, conn.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func (s *apiServer) routes(mux *http.ServeMux) {
	api := http.NewServeMux()
	api.HandleFunc("/api/connect", s.handleConnect)
	api.HandleFunc("/api/disconnect", s.handleDisconnect)
	api.HandleFunc("/api/vcenters", s.handleVCenters)
	api.HandleFunc("/api/vms", s.handleVMs)
	api.HandleFunc("/api/disks", s.handleDisks)
	api.HandleFunc("/api/disk-limit", s.handleSetLimit)
	api.HandleFunc("/api/reset-all", s.handleResetAll)
	api.HandleFunc("/api/resourcepools", s.handleResourcePools)
	api.HandleFunc("/api/resourcepool-vms", s.handleResourcePoolVMs)
	api.HandleFunc("/api/resourcepool-reset-all", s.handleResourcePoolResetAll)
	api.HandleFunc("/api/datastores", s.handleDatastores)
	api.HandleFunc("/api/datastore-vms", s.handleDatastoreVMs)
	api.HandleFunc("/api/datastore-reset-all", s.handleDatastoreResetAll)
	api.HandleFunc("/api/check-paths", s.handleCheckPaths)

	mux.Handle("/api/", s.authMiddleware(api))
}

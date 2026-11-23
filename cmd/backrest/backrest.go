package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	v1 "github.com/baragoon/backrest/gen/go/v1"
	"github.com/baragoon/backrest/gen/go/v1/v1connect"
	"github.com/baragoon/backrest/gen/go/v1sync/v1syncconnect"
	"github.com/baragoon/backrest/internal/api"
	syncapi "github.com/baragoon/backrest/internal/api/syncapi"
	"github.com/baragoon/backrest/internal/auth"
	"github.com/baragoon/backrest/internal/config"
	"github.com/baragoon/backrest/internal/env"
	"github.com/baragoon/backrest/internal/kvstore"
	"github.com/baragoon/backrest/internal/logstore"
	"github.com/baragoon/backrest/internal/metric"
	"github.com/baragoon/backrest/internal/oplog"
	"github.com/baragoon/backrest/internal/oplog/sqlitestore"
	"github.com/baragoon/backrest/internal/orchestrator"
	"github.com/baragoon/backrest/internal/resticinstaller"
	"github.com/baragoon/backrest/webui"
	"github.com/mattn/go-colorable"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"gopkg.in/natefinch/lumberjack.v2"
)

var installDepsOnly = flag.Bool("install-deps-only", false, "install dependencies and exit")
var printVersion = flag.Bool("version", false, "print version and exit")

var (
	version = "unknown"
	commit  = "unknown"

	healthBindAddressMu sync.Mutex
)

const (
	healthCheckPath                 = "/healthz"
	healthCheckResponse             = "backrest"
	healthBindAddressFileNamePrefix = "backrest-health-bind-address-"
	healthBindAddressLockTimeout    = 2 * time.Second
)

type healthBindAddressMetadata struct {
	PID     uint32 `json:"pid"`
	StartID string `json:"start_id"`
	Address string `json:"address"`
}

// onOpLogReady, when set, is invoked with the OpLog once it is constructed.
// The tray build (-tags tray) uses it to drive a status-reflecting menu bar
// icon. It stays nil in non-tray builds, so this adds no behavior there.
var onOpLogReady func(*oplog.OpLog)

func isHealthCommand() bool {
	return len(flag.Args()) > 0 && flag.Arg(0) == "health"
}

func runApp() {
	flag.Parse()
	if isHealthCommand() {
		address, err := healthBindAddress()
		if err == nil {
			err = checkHealth(address)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *printVersion {
		fmt.Printf("backrest version: %s, commit: %s\n", version, commit)
		os.Exit(0)
	}
	installLoggers(version, commit)

	// Install dependencies if requested
	resticPath, err := resticinstaller.FindOrInstallResticBinary()
	if err != nil {
		zap.L().Fatal("error finding or installing restic", zap.Error(err))
	}
	if *installDepsOnly {
		zap.L().Info("dependencies installed, exiting")
		return
	}
	bindAddress := env.BindAddress()

	// Setup context and signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go onterm(os.Interrupt, cancel)
	go onterm(os.Interrupt, newForceKillHandler())

	// Create dependency components
	configMgr := &config.ConfigManager{Store: createConfigStore()}
	cfg, err := configMgr.Get()
	if err != nil {
		zap.L().Fatal("error loading config", zap.Error(err))
	}

	opLog, opLogStore, err := newOpLog(cfg)
	if err != nil {
		zap.L().Fatal("error creating oplog", zap.Error(err))
	}
	defer opLogStore.Close()

	// onOpLogReady lets the system tray (when built with -tags tray) observe
	// operation status in-process to reflect backup state in its icon. It is
	// nil in non-tray builds.
	if onOpLogReady != nil {
		onOpLogReady(opLog)
	}

	logStore, unsubscribeLogStore, err := newLogStore(opLog)
	if err != nil {
		zap.L().Fatal("error creating log store", zap.Error(err))
	}
	defer unsubscribeLogStore()
	defer func() {
		if err := logStore.Close(); err != nil {
			zap.L().Warn("error closing log store", zap.Error(err))
		}
	}()

	orch, err := orchestrator.NewOrchestrator(resticPath, configMgr, opLog, logStore)
	if err != nil {
		zap.L().Fatal("error creating orchestrator", zap.Error(err))
	}

	kvdbPath := filepath.Join(env.DataDir(), "kvdb.sqlite")
	sharedKvdb, err := kvstore.NewSqliteDbForKvStore(kvdbPath)
	if err != nil {
		zap.L().Fatal("error creating general kvstore database pool", zap.Error(err))
	}
	defer sharedKvdb.Close()

	peerStateManager, err := syncapi.NewSqlitePeerStateManager(sharedKvdb)
	if err != nil {
		zap.L().Fatal("error creating peer state manager", zap.Error(err))
	}
	syncMgr := syncapi.NewSyncManager(configMgr, opLog, orch, peerStateManager)
	authenticator := newAuthenticator(configMgr)

	// Start background services
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		orch.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		syncMgr.RunSync(ctx)
	}()

	// Setup and start HTTP server
	server := newServer(configMgr, peerStateManager, orch, opLog, logStore, syncMgr, authenticator, bindAddress)
	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()

	zap.L().Info("starting web server", zap.String("addr", server.Addr))
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		zap.L().Fatal("error starting server", zap.Error(err))
		return
	}
	actualAddress := listener.Addr().String()
	if err := writeHealthBindAddress(actualAddress); err != nil {
		zap.L().Warn("error recording healthcheck bind address", zap.Error(err))
	} else {
		defer removeHealthBindAddress(actualAddress)
	}
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		zap.L().Error("error starting server", zap.Error(err))
	}
	zap.L().Info("HTTP gateway shutdown")

	wg.Wait()
}

func checkHealth(address string) error {
	probeURL, err := healthProbeURL(address)
	if err != nil {
		return err
	}
	client := http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Get(probeURL.String())
	if err != nil {
		return fmt.Errorf("Backrest healthcheck failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Backrest healthcheck failed: HTTP status %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(len(healthCheckResponse)+1)))
	if err != nil {
		return fmt.Errorf("reading Backrest healthcheck response: %w", err)
	}
	if string(body) != healthCheckResponse {
		return fmt.Errorf("Backrest healthcheck failed: unexpected response body %q", body)
	}
	return nil
}

func healthProbeURL(address string) (*url.URL, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid Backrest bind address %q: %w", address, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}

	probeURL := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, port),
		Path:   healthCheckPath,
	}
	return &probeURL, nil
}

func healthBindAddress() (string, error) {
	if bindAddressFlag := flag.Lookup("bind-address"); bindAddressFlag != nil && bindAddressFlag.Value.String() != "" {
		return env.BindAddress(), nil
	}
	if path := os.Getenv("BACKREST_HEALTH_BIND_ADDRESS_FILE"); path != "" {
		address, active, err := readHealthBindAddress(path, 0)
		if err != nil {
			return "", err
		}
		if active {
			return address, nil
		}
		return env.BindAddress(), nil
	}
	if os.Getenv("BACKREST_HEALTHCHECK") != "1" {
		return env.BindAddress(), nil
	}

	tempDir := healthTempDir()
	entries, err := os.ReadDir(tempDir)
	if errors.Is(err, os.ErrNotExist) {
		return env.BindAddress(), nil
	}
	if err != nil {
		return "", fmt.Errorf("reading healthcheck bind address directory: %w", err)
	}
	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, healthBindAddressFileNamePrefix) || !entry.Type().IsRegular() {
			continue
		}
		pidText, _, ok := strings.Cut(strings.TrimPrefix(name, healthBindAddressFileNamePrefix), "-")
		if !ok {
			continue
		}
		pid, err := strconv.ParseUint(pidText, 10, 64)
		if err != nil || !healthPIDInRange(pid) {
			continue
		}
		address, active, err := readHealthBindAddress(filepath.Join(tempDir, name), uint32(pid))
		if err != nil {
			// An unsolicited record must not prevent discovery of other instances.
			continue
		}
		if active {
			paths = append(paths, address)
		}
	}
	if len(paths) == 0 {
		return env.BindAddress(), nil
	}
	if len(paths) > 1 {
		return "", fmt.Errorf("multiple Backrest health address files found; set BACKREST_HEALTH_BIND_ADDRESS_FILE or --bind-address")
	}
	return paths[0], nil
}

func readHealthBindAddress(path string, expectedPID uint32) (string, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading healthcheck bind address: %w", err)
	}
	var metadata healthBindAddressMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", false, fmt.Errorf("decoding healthcheck bind address: %w", err)
	}
	if !healthPIDInRange(uint64(metadata.PID)) || metadata.StartID == "" || metadata.Address == "" {
		return "", false, fmt.Errorf("invalid healthcheck bind address metadata in %q", path)
	}
	if expectedPID != 0 && metadata.PID != expectedPID {
		return "", false, nil
	}
	startID, running, err := healthProcessIdentity(metadata.PID)
	if err != nil {
		return "", false, fmt.Errorf("checking healthcheck process %d: %w", metadata.PID, err)
	}
	if !running || startID != metadata.StartID {
		return "", false, nil
	}
	if _, err := healthProbeURL(metadata.Address); err != nil {
		return "", false, err
	}
	return metadata.Address, true, nil
}

func writeHealthBindAddress(address string) error {
	pid := uint32(os.Getpid())
	path, enabled := healthBindAddressFile(pid, address)
	if !enabled {
		return nil
	}
	startID, running, err := healthProcessIdentity(pid)
	if err != nil {
		return fmt.Errorf("getting healthcheck process start identifier: %w", err)
	}
	if !running || startID == "" {
		return fmt.Errorf("getting healthcheck process start identifier: process %d is not running", pid)
	}
	data, err := json.Marshal(healthBindAddressMetadata{
		PID:     pid,
		StartID: startID,
		Address: address,
	})
	if err != nil {
		return fmt.Errorf("encoding healthcheck bind address: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating healthcheck bind address directory: %w", err)
	}

	// Keep the existing creation mode and let the umask apply.
	tmp, err := os.OpenFile(filepath.Join(dir, ".backrest-health-"+rand.Text()), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("writing healthcheck bind address: %w", err)
	}
	_, err = tmp.Write(data)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = withHealthBindAddressLock(path, func() error {
			return os.Rename(tmp.Name(), path)
		})
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing healthcheck bind address: %w", err)
	}
	return nil
}

func healthBindAddressFile(pid uint32, address string) (string, bool) {
	if path := os.Getenv("BACKREST_HEALTH_BIND_ADDRESS_FILE"); path != "" {
		return path, true
	}
	if os.Getenv("BACKREST_HEALTHCHECK") != "1" {
		return "", false
	}
	tempDir := os.Getenv("TMPDIR")
	if tempDir == "" {
		tempDir = "/tmp"
	}
	hash := sha256.Sum256([]byte(address))
	name := fmt.Sprintf("%s%d-%s", healthBindAddressFileNamePrefix, pid, hex.EncodeToString(hash[:]))
	return filepath.Join(tempDir, name), true
}

func healthTempDir() string {
	if tempDir := os.Getenv("TMPDIR"); tempDir != "" {
		return tempDir
	}
	return "/tmp"
}

func removeHealthBindAddress(address string) {
	pid := uint32(os.Getpid())
	path, enabled := healthBindAddressFile(pid, address)
	if !enabled {
		return
	}
	err := withHealthBindAddressLock(path, func() error {
		currentAddress, owned, err := readHealthBindAddress(path, pid)
		if err != nil {
			return err
		}
		if !owned || currentAddress != address {
			return nil
		}
		return os.Remove(path)
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		zap.L().Warn("error removing healthcheck bind address", zap.Error(err))
	}
}

func healthBindAddressLockPath(path string) (string, error) {
	if lockPath := os.Getenv("BACKREST_HEALTH_BIND_ADDRESS_LOCK_FILE"); lockPath != "" {
		return lockPath, nil
	}
	dir, err := privateHealthLockDir()
	if err != nil {
		return "", err
	}
	// Serialize updates to records in the same directory.
	recordDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	recordDir, err = filepath.EvalSymlinks(recordDir)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		recordDir = strings.ToLower(recordDir)
	}
	hash := sha256.Sum256([]byte(recordDir))
	return filepath.Join(dir, hex.EncodeToString(hash[:])+".lock"), nil
}

func withHealthBindAddressLock(path string, fn func() error) (err error) {
	healthBindAddressMu.Lock()
	defer healthBindAddressMu.Unlock()

	lockPath, err := healthBindAddressLockPath(path)
	if err != nil {
		return fmt.Errorf("locating healthcheck bind address lock: %w", err)
	}
	info, err := os.Lstat(lockPath)
	if err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("healthcheck lock %q is not a regular file", lockPath)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking healthcheck bind address lock: %w", err)
	}
	lock, err := openHealthLock(lockPath)
	if err != nil {
		return fmt.Errorf("opening healthcheck bind address lock: %w", err)
	}
	defer lock.close()

	ctx, cancel := context.WithTimeout(context.Background(), healthBindAddressLockTimeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		locked, err := lock.tryLock()
		if err != nil {
			return fmt.Errorf("locking healthcheck bind address: %w", err)
		}
		if locked {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("locking healthcheck bind address: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	defer func() {
		if unlockErr := lock.unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("unlocking healthcheck bind address: %w", unlockErr))
		}
	}()
	return fn()
}

func createConfigStore() config.ConfigStore {
	return &config.JsonFileStore{Path: env.ConfigFilePath()}
}

func newOpLog(cfg *v1.Config) (*oplog.OpLog, *sqlitestore.SqliteStore, error) {
	oplogFile := filepath.Join(env.DataDir(), "oplog.sqlite")
	opstore, err := sqlitestore.NewSqliteStore(oplogFile)
	if errors.Is(err, sqlitestore.ErrLocked) {
		zap.L().Fatal("oplog is locked by another instance of backrest", zap.String("data_dir", env.DataDir()))
	} else if err != nil {
		zap.L().Warn("operation log may be corrupted, if errors recur delete the file and restart. Your backups stored in your repos are safe.", zap.String("oplog_file", oplogFile))
		return nil, nil, err
	}
	migratePopulateGuids(opstore, cfg)
	log, err := oplog.NewOpLog(opstore)
	if err != nil {
		opstore.Close()
		return nil, nil, err
	}
	if err := oplog.ApplyMigrations(log); err != nil {
		zap.S().Fatalf("error applying oplog migrations: %v", err)
	}
	return log, opstore, nil
}

func newLogStore(opLog *oplog.OpLog) (*logstore.LogStore, func(), error) {
	logStore, err := logstore.NewLogStore(filepath.Join(env.DataDir(), "tasklogs"))
	if err != nil {
		return nil, nil, err
	}
	logstore.MigrateTarLogsInDir(logStore, filepath.Join(env.DataDir(), "rotatinglogs"))

	deleteLogsForOp := func(ops []*v1.Operation, event oplog.OperationEvent) {
		if event != oplog.OPERATION_DELETED {
			return
		}
		for _, op := range ops {
			if err := logStore.DeleteWithParent(op.Id); err != nil {
				zap.L().Warn("error deleting logs for operation", zap.Int64("op_id", op.Id), zap.Error(err))
			}
		}
	}
	opLog.Subscribe(oplog.Query{}, &deleteLogsForOp)

	unsubscribe := func() {
		opLog.Unsubscribe(&deleteLogsForOp)
	}

	return logStore, unsubscribe, nil
}

func newAuthenticator(configMgr *config.ConfigManager) *auth.Authenticator {
	secretFile := filepath.Join(env.DataDir(), "jwt-secret")
	data, err := os.ReadFile(secretFile)
	if err != nil {
		zap.L().Info("generating new auth secret")
		secret := make([]byte, 64)
		if n, err := rand.Read(secret); err != nil || n != 64 {
			zap.L().Fatal("error generating secret", zap.Error(err))
		}
		if err := os.MkdirAll(env.DataDir(), 0700); err != nil {
			zap.L().Fatal("error creating data directory", zap.Error(err))
		}
		if err := os.WriteFile(secretFile, secret, 0600); err != nil {
			zap.L().Fatal("error writing secret to file", zap.Error(err))
		}
		data = secret
	} else {
		zap.L().Debug("loading auth secret from file")
	}
	return auth.NewAuthenticator(data, configMgr)
}

func newServer(
	configMgr *config.ConfigManager,
	peerStateManager syncapi.PeerStateManager,
	orch *orchestrator.Orchestrator,
	opLog *oplog.OpLog,
	logStore *logstore.LogStore,
	syncMgr *syncapi.SyncManager,
	authenticator *auth.Authenticator,
	bindAddress string,
) *http.Server {
	// API Handlers
	apiBackrestHandler := api.NewBackrestHandler(configMgr, peerStateManager, orch, opLog, logStore)
	apiAuthenticationHandler := api.NewAuthenticationHandler(authenticator)
	syncHandler := syncapi.NewBackrestSyncHandler(syncMgr)
	syncStateHandler := syncapi.NewBackrestSyncStateHandler(syncMgr)
	downloadHandler := api.NewDownloadHandler(opLog, orch)

	// Routing
	rootMux := newRootMux(apiBackrestHandler, apiAuthenticationHandler, syncHandler, syncStateHandler, downloadHandler, authenticator)

	var handler http.Handler = rootMux
	if version == "unknown" { // dev build, enable CORS for local development
		handler = newCorsMiddleware(rootMux)
	}

	return &http.Server{
		Addr:    bindAddress,
		Handler: h2c.NewHandler(handler, &http2.Server{}),
	}
}

func newRootMux(
	apiBackrestHandler v1connect.BackrestHandler,
	apiAuthenticationHandler v1connect.AuthenticationHandler,
	syncHandler v1syncconnect.BackrestSyncServiceHandler,
	syncStateHandler v1syncconnect.BackrestSyncStateServiceHandler,
	downloadHandler http.Handler,
	authenticator *auth.Authenticator,
) *http.ServeMux {
	// Authenticated routes
	authedMux := http.NewServeMux()
	backrestPath, backrestHandler := v1connect.NewBackrestHandler(apiBackrestHandler)
	authedMux.Handle(backrestPath, backrestHandler)
	syncStatePath, syncStateHandlerUnauthed := v1syncconnect.NewBackrestSyncStateServiceHandler(syncStateHandler)
	authedMux.Handle(syncStatePath, syncStateHandlerUnauthed)
	authedMux.Handle("/metrics", metric.GetRegistry().Handler())

	// Unauthenticated routes
	unauthedMux := http.NewServeMux()
	authPath, authHandler := v1connect.NewAuthenticationHandler(apiAuthenticationHandler)
	unauthedMux.Handle(authPath, authHandler)
	syncPath, syncHandlerUnauthed := v1syncconnect.NewBackrestSyncServiceHandler(syncHandler)
	unauthedMux.Handle(syncPath, syncHandlerUnauthed)
	unauthedMux.Handle("/download/", http.StripPrefix("/download", downloadHandler))

	// Root mux to dispatch to authenticated or unauthenticated handlers
	rootMux := http.NewServeMux()
	rootMux.HandleFunc(healthCheckPath, backrestHealthHandler)

	// Create a fall through handler which tries the muxes in order to find one to handle the route.
	rootMux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if unauthedMux has a handler for this path
		handler, pattern := unauthedMux.Handler(r)
		if pattern != "" {
			handler.ServeHTTP(w, r)
			return
		}

		// Check if the mux can provide a handler for the authenticated routes
		handler, pattern = authedMux.Handler(r)
		if pattern != "" {
			auth.RequireAuthentication(handler, authenticator).ServeHTTP(w, r)
			return
		}

		// Fall back to web UI handler.
		webui.Handler().ServeHTTP(w, r)
	}))

	return rootMux
}

func backrestHealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, healthCheckResponse)
}

func newCorsMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Authorization, Accept-Encoding")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func onterm(s os.Signal, callback func()) {
	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, s, syscall.SIGTERM)
	for {
		<-sigchan
		callback()
	}
}

func newForceKillHandler() func() {
	var times atomic.Int32
	return func() {
		if times.Load() > 0 {
			buf := make([]byte, 1<<16)
			runtime.Stack(buf, true)
			os.Stderr.Write(buf)
			zap.L().Fatal("dumped all running coroutine stack traces, forcing termination")
		}
		times.Add(1)
		zap.L().Warn("attempting graceful shutdown, to force termination press Ctrl+C again")
	}
}

func installLoggers(version, commit string) {
	// Pretty logging for console
	c := zap.NewDevelopmentEncoderConfig()
	c.EncodeLevel = zapcore.CapitalColorLevelEncoder
	c.EncodeTime = zapcore.ISO8601TimeEncoder

	logLevel := zapcore.InfoLevel
	if version == "unknown" { // dev build
		logLevel = zapcore.DebugLevel
	}
	pretty := zapcore.NewCore(
		zapcore.NewConsoleEncoder(c),
		zapcore.AddSync(colorable.NewColorableStdout()),
		logLevel,
	)

	// JSON logging to log directory
	logsDir := env.LogsPath()
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		zap.ReplaceGlobals(zap.New(pretty))
		zap.L().Error("error creating logs directory, will only log to console for now", zap.String("path", logsDir), zap.Error(err))
		return
	}

	writer := &lumberjack.Logger{
		Filename:   filepath.Join(logsDir, "backrest.log"),
		MaxSize:    5, // megabytes
		MaxBackups: 3,
		MaxAge:     14,
		Compress:   true,
	}

	ugly := zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(writer),
		zapcore.DebugLevel,
	)

	zap.ReplaceGlobals(zap.New(zapcore.NewTee(pretty, ugly)))
	zap.L().Info("backrest starting", zap.String("version", version), zap.String("commit", commit), zap.String("log_dir", logsDir))
}

func migratePopulateGuids(logstore oplog.OpStore, cfg *v1.Config) {
	repoToGUID := make(map[string]string)
	for _, repo := range cfg.Repos {
		if repo.Guid != "" {
			repoToGUID[repo.Id] = repo.Guid
		}
	}
	getGuid := func(id string) string {
		if guid, ok := repoToGUID[id]; ok {
			return guid
		}
		h := sha256.New()
		h.Write([]byte(id))
		newGuid := hex.EncodeToString(h.Sum(nil))
		repoToGUID[id] = newGuid
		return newGuid
	}

	migratedOpCount := 0
	inscopeOperations := 0
	if err := logstore.Transform(oplog.Query{}.SetRepoGUID(""), func(op *v1.Operation) (*v1.Operation, error) {
		inscopeOperations++
		if op.RepoGuid != "" {
			return nil, nil
		}
		op.RepoGuid = getGuid(op.RepoId)
		migratedOpCount++
		return op, nil
	}); err != nil {
		zap.L().Fatal("error populating repo GUIDs for existing operations", zap.Error(err))
	} else if migratedOpCount > 0 {
		zap.L().Info("populated repo GUIDs for existing operations", zap.Int("count", migratedOpCount))
	}
}

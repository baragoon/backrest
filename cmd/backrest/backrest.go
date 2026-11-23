package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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
)

// onOpLogReady, when set, is invoked with the OpLog once it is constructed.
// The tray build (-tags tray) uses it to drive a status-reflecting menu bar
// icon. It stays nil in non-tray builds, so this adds no behavior there.
var onOpLogReady func(*oplog.OpLog)

func runApp() {
	flag.Parse()
	if len(flag.Args()) > 0 && flag.Arg(0) == "health" {
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
	if err := writeHealthBindAddress(bindAddress); err != nil {
		zap.L().Fatal("error recording healthcheck bind address", zap.Error(err))
	}

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
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		zap.L().Error("error starting server", zap.Error(err))
	}
	zap.L().Info("HTTP gateway shutdown")

	wg.Wait()
}

func checkHealth(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid Backrest bind address %q: %w", address, err)
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
		Path:   "/",
	}
	client := http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	response, err := client.Get(probeURL.String())
	if err != nil {
		return fmt.Errorf("Backrest healthcheck failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("Backrest healthcheck failed: HTTP status %s", response.Status)
	}
	return nil
}

func healthBindAddress() (string, error) {
	if bindAddressFlag := flag.Lookup("bind-address"); bindAddressFlag != nil && bindAddressFlag.Value.String() != "" {
		return env.BindAddress(), nil
	}
	path, enabled := healthBindAddressFile()
	if !enabled {
		return env.BindAddress(), nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return env.BindAddress(), nil
	}
	if err != nil {
		return "", fmt.Errorf("reading healthcheck bind address: %w", err)
	}
	address := strings.TrimSpace(string(data))
	if address == "" {
		return "", fmt.Errorf("healthcheck bind address file %q is empty", path)
	}
	return address, nil
}

func writeHealthBindAddress(address string) error {
	path, enabled := healthBindAddressFile()
	if !enabled {
		return nil
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
	_, err = tmp.WriteString(address)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing healthcheck bind address: %w", err)
	}
	return nil
}

func healthBindAddressFile() (string, bool) {
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
	return filepath.Join(tempDir, "backrest-health-bind-address"), true
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

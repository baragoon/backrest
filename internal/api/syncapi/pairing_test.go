package syncapi

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	v1 "github.com/baragoon/backrest/gen/go/v1"
	"github.com/baragoon/backrest/gen/go/v1sync"
	"github.com/baragoon/backrest/internal/config"
	"github.com/baragoon/backrest/internal/config/migrations"
	"github.com/baragoon/backrest/internal/cryptoutil"
	"github.com/baragoon/backrest/internal/testutil"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/proto"
)

func TestValidatePairingSecret(t *testing.T) {
	now := time.Unix(1000, 0)

	tokens := []*v1.Multihost_PairingToken{
		{
			Secret:        "valid-secret",
			Label:         "test-token",
			CreatedAtUnix: 900,
			ExpiresAtUnix: 2000,
			MaxUses:       3,
			Uses:          1,
		},
		{
			Secret:        "unlimited-token",
			Label:         "unlimited",
			CreatedAtUnix: 900,
			ExpiresAtUnix: 0, // no expiry
			MaxUses:       0, // unlimited uses
			Uses:          100,
		},
	}

	tests := []struct {
		name      string
		secret    string
		tokens    []*v1.Multihost_PairingToken
		now       time.Time
		wantLabel string
		wantErr   bool
	}{
		{
			name:      "valid secret",
			secret:    "valid-secret",
			tokens:    tokens,
			now:       now,
			wantLabel: "test-token",
		},
		{
			name:      "unlimited token",
			secret:    "unlimited-token",
			tokens:    tokens,
			now:       now,
			wantLabel: "unlimited",
		},
		{
			name:    "empty secret",
			secret:  "",
			tokens:  tokens,
			now:     now,
			wantErr: true,
		},
		{
			name:    "wrong secret",
			secret:  "wrong-secret",
			tokens:  tokens,
			now:     now,
			wantErr: true,
		},
		{
			name:   "expired token",
			secret: "valid-secret",
			tokens: []*v1.Multihost_PairingToken{
				{
					Secret:        "valid-secret",
					Label:         "expired",
					ExpiresAtUnix: 500,
					MaxUses:       0,
				},
			},
			now:     now,
			wantErr: true,
		},
		{
			name:   "max uses reached",
			secret: "valid-secret",
			tokens: []*v1.Multihost_PairingToken{
				{
					Secret:  "valid-secret",
					Label:   "exhausted",
					MaxUses: 2,
					Uses:    2,
				},
			},
			now:     now,
			wantErr: true,
		},
		{
			name:    "nil tokens",
			secret:  "anything",
			tokens:  nil,
			now:     now,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			token, err := ValidatePairingSecret(tc.secret, tc.tokens, tc.now)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if token.Label != tc.wantLabel {
				t.Errorf("label = %q, want %q", token.Label, tc.wantLabel)
			}
		})
	}
}

func TestPairingTokenFlow(t *testing.T) {
	testutil.InstallZapLogger(t)

	peerHostAddr := newSyncListener(t)
	peerClientAddr := newSyncListener(t)

	pairingSecret, err := cryptoutil.GeneratePairingSecret()
	if err != nil {
		t.Fatalf("failed to generate pairing secret: %v", err)
	}

	// Host has a pairing token but NO authorized clients yet.
	peerHostConfig := &v1.Config{
		Version:  migrations.CurrentVersion,
		Instance: defaultHostID,
		Repos:    []*v1.Repo{},
		Multihost: &v1.Multihost{
			Identity:          identity1,
			AuthorizedClients: []*v1.Multihost_Peer{}, // empty — client not pre-authorized
			PairingTokens: []*v1.Multihost_PairingToken{
				{
					Secret:        pairingSecret,
					Label:         "test-pairing",
					CreatedAtUnix: time.Now().Unix(),
					ExpiresAtUnix: time.Now().Add(1 * time.Hour).Unix(),
					MaxUses:       1,
					Uses:          0,
				},
			},
		},
	}

	// Client knows about the host and has the pairing secret.
	peerClientConfig := &v1.Config{
		Version:  migrations.CurrentVersion,
		Instance: defaultClientID,
		Repos:    []*v1.Repo{},
		Multihost: &v1.Multihost{
			Identity: identity2,
			KnownHosts: []*v1.Multihost_Peer{
				{
					Keyid:                identity1.Keyid,
					InstanceId:           defaultHostID,
					InstanceUrl:          fmt.Sprintf("http://%s", peerHostAddr.Addr()),
					InitialPairingSecret: pairingSecret,
				},
			},
		},
	}

	peerHost := newPeerUnderTest(t, peerHostConfig)
	peerClient := newPeerUnderTest(t, peerClientConfig)

	startRunningSyncAPI(t, peerHost, peerHostAddr)
	startRunningSyncAPI(t, peerClient, peerClientAddr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The client should successfully connect via the pairing token.
	tryConnect(t, ctx, peerClient, peerClientConfig.Multihost.KnownHosts[0])

	// The server pairs the client during its own runSync handshake handling, which
	// runs concurrently with — and may finish slightly after — the client reaching
	// CONNECTED state. Poll for the host config to reflect the pairing.
	var hostConfig *v1.Config
	testutil.Try(t, ctx, func() error {
		var err error
		hostConfig, err = peerHost.configMgr.Get()
		if err != nil {
			return fmt.Errorf("get host config: %w", err)
		}
		if len(hostConfig.Multihost.AuthorizedClients) != 1 {
			return fmt.Errorf("expected 1 authorized client, got %d", len(hostConfig.Multihost.AuthorizedClients))
		}
		return nil
	})

	ac := hostConfig.Multihost.AuthorizedClients[0]
	if ac.Keyid != identity2.Keyid {
		t.Errorf("authorized client keyid = %q, want %q", ac.Keyid, identity2.Keyid)
	}
	if ac.InstanceId != defaultClientID {
		t.Errorf("authorized client instance id = %q, want %q", ac.InstanceId, defaultClientID)
	}

	// Verify the pairing token was consumed (max_uses=1, so it should be removed).
	if len(hostConfig.Multihost.PairingTokens) != 0 {
		t.Errorf("expected 0 pairing tokens after consumption, got %d", len(hostConfig.Multihost.PairingTokens))
	}

	testutil.Try(t, ctx, func() error {
		cfg, err := peerClient.configMgr.Get()
		if err != nil {
			return err
		}
		if cfg.Multihost.KnownHosts[0].InitialPairingSecret != "" {
			return fmt.Errorf("pairing secret not cleared after authorization")
		}
		return nil
	})
}

func TestPairingSecretClearedOnlyAfterServerConfirmation(t *testing.T) {
	for _, replaceSecret := range []bool{false, true} {
		t.Run(fmt.Sprintf("replaceSecret=%t", replaceSecret), func(t *testing.T) {
			host := &v1.Multihost_Peer{Keyid: identity1.Keyid, InstanceId: defaultHostID, InstanceUrl: "http://127.0.0.1:1234", InitialPairingSecret: "initial-secret"}
			cfg := &v1.Config{
				Version:  migrations.CurrentVersion,
				Instance: defaultClientID,
				Multihost: &v1.Multihost{
					Identity:   identity2,
					KnownHosts: []*v1.Multihost_Peer{host},
				},
			}
			client := newPeerUnderTest(t, cfg)
			snapshot, err := client.configMgr.Get()
			if err != nil {
				t.Fatal(err)
			}
			handler := newSyncHandlerClient(zap.NewNop(), client.manager, syncConfigSnapshot{config: snapshot}, client.oplog, snapshot.Multihost.KnownHosts[0])
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer handler.OnConnectionDisconnected()
			stream := newBidiSyncCommandStream()
			if err := handler.OnConnectionEstablished(ctx, stream, handler.peer); err != nil {
				t.Fatal(err)
			}
			current, err := client.configMgr.Get()
			if err != nil {
				t.Fatal(err)
			}
			if got := current.Multihost.KnownHosts[0].InitialPairingSecret; got != "initial-secret" {
				t.Fatalf("secret after handshake = %q, want initial-secret", got)
			}
			if err := handler.HandleReceiveConfig(ctx, stream, &v1sync.SyncStreamItem_SyncActionReceiveConfig{}); err == nil {
				t.Fatal("expected invalid remote config to be rejected")
			}
			current, _ = client.configMgr.Get()
			if got := current.Multihost.KnownHosts[0].InitialPairingSecret; got != "initial-secret" {
				t.Fatalf("secret after invalid confirmation = %q, want initial-secret", got)
			}
			if replaceSecret {
				if err := client.configMgr.Transform(func(cfg *v1.Config) (*v1.Config, error) {
					cfg.Multihost.KnownHosts[0].InitialPairingSecret = "replacement-secret"
					return cfg, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := handler.HandleReceiveConfig(ctx, stream, &v1sync.SyncStreamItem_SyncActionReceiveConfig{Config: &v1sync.RemoteConfig{}}); err != nil {
				t.Fatal(err)
			}
			current, _ = client.configMgr.Get()
			want := ""
			if replaceSecret {
				want = "replacement-secret"
			}
			if got := current.Multihost.KnownHosts[0].InitialPairingSecret; got != want {
				t.Fatalf("secret after confirmation = %q, want %q", got, want)
			}
		})
	}
}

func TestPairingWithStaleServerSnapshot(t *testing.T) {
	cfg := &v1.Config{
		Version:   migrations.CurrentVersion,
		Instance:  defaultHostID,
		Multihost: &v1.Multihost{Identity: identity1},
	}
	host := newPeerUnderTest(t, cfg)
	snapshotConfig, err := host.configMgr.Get()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshotConfig.Multihost.AuthorizedClients) != 0 {
		t.Fatal("expected the request-time snapshot to have no authorized clients")
	}
	authorizedPeer := &v1.Multihost_Peer{
		Keyid: identity2.Keyid, InstanceId: defaultClientID,
		Permissions: []*v1.Multihost_Permission{{Type: v1.Multihost_Permission_PERMISSION_READ_CONFIG}},
	}
	if err := host.configMgr.Transform(func(cfg *v1.Config) (*v1.Config, error) {
		cfg.Multihost.AuthorizedClients = append(cfg.Multihost.AuthorizedClients, authorizedPeer)
		return cfg, nil
	}); err != nil {
		t.Fatal(err)
	}
	key, err := cryptoutil.NewPrivateKey(identity2)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := createHandshakePacket(defaultClientID, key, "", freshTranscript(t))
	if err != nil {
		t.Fatal(err)
	}
	pairing := (&BackrestSyncHandler{mgr: host.manager}).resolveClientPeer()
	peer, err := pairing(packet)
	if err != nil {
		t.Fatalf("authorized client rejected with stale snapshot: %v", err)
	}
	if !proto.Equal(peer, authorizedPeer) {
		t.Fatalf("returned peer = %v, want current authorized record %v", peer, authorizedPeer)
	}
	if err := authorizeHandshakeAsPeer(packet, peer); err != nil {
		t.Fatal(err)
	}

	unknownIdentity, err := cryptoutil.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	unknownKey, err := cryptoutil.NewPrivateKey(unknownIdentity)
	if err != nil {
		t.Fatal(err)
	}
	unknownPacket, err := createHandshakePacket("unknown-client", unknownKey, "", freshTranscript(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pairing(unknownPacket); err == nil {
		t.Fatal("expected unlisted identity without a pairing secret to be rejected")
	}

	current, err := host.configMgr.Get()
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Multihost.AuthorizedClients) != 1 {
		t.Fatalf("authorized clients = %d, want 1", len(current.Multihost.AuthorizedClients))
	}
}

func TestServerUsesCurrentConfigForInitialState(t *testing.T) {
	testutil.InstallZapLogger(t)

	host := newPeerUnderTest(t, &v1.Config{
		Version:   migrations.CurrentVersion,
		Instance:  defaultHostID,
		Multihost: &v1.Multihost{Identity: identity1},
	})
	staleConfig, err := host.configMgr.Get()
	if err != nil {
		t.Fatal(err)
	}
	authorizedPeer := &v1.Multihost_Peer{
		Keyid:      identity2.Keyid,
		InstanceId: defaultClientID,
		Permissions: []*v1.Multihost_Permission{{
			Type:   v1.Multihost_Permission_PERMISSION_READ_CONFIG,
			Scopes: []string{"repo:" + defaultRepoID},
		}},
	}
	if err := host.configMgr.Transform(func(cfg *v1.Config) (*v1.Config, error) {
		cfg.Repos = []*v1.Repo{{
			Id:       defaultRepoID,
			Guid:     defaultRepoGUID,
			Uri:      "test-uri",
			Password: "current-password",
		}}
		cfg.Multihost.AuthorizedClients = []*v1.Multihost_Peer{authorizedPeer}
		return cfg, nil
	}); err != nil {
		t.Fatal(err)
	}

	handler := newSyncHandlerServer(host.manager, &syncConfigSnapshot{config: staleConfig}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer handler.OnConnectionDisconnected()
	stream := newBidiSyncCommandStream()
	if err := handler.OnConnectionEstablished(ctx, stream, authorizedPeer); err != nil {
		t.Fatalf("establish connection with current authorization: %v", err)
	}

	initialConfig := (<-stream.sendChan).GetReceiveConfig().GetConfig()
	if len(initialConfig.GetRepos()) != 1 || initialConfig.GetRepos()[0].GetPassword() != "current-password" {
		t.Fatalf("initial config = %v, want the current repository config", initialConfig)
	}
}

func TestServerInitialStateRevalidatesAuthorization(t *testing.T) {
	for _, revoked := range []bool{true, false} {
		t.Run(fmt.Sprintf("revoked=%t", revoked), func(t *testing.T) {
			peer := &v1.Multihost_Peer{
				Keyid: identity2.Keyid, InstanceId: defaultClientID,
				Permissions: []*v1.Multihost_Permission{
					{Type: v1.Multihost_Permission_PERMISSION_READ_CONFIG, Scopes: []string{"repo:" + defaultRepoID}},
					{Type: v1.Multihost_Permission_PERMISSION_RECEIVE_SHARED_REPOS},
				},
			}
			host := newPeerUnderTest(t, &v1.Config{
				Version:  migrations.CurrentVersion,
				Instance: defaultHostID,
				Repos: []*v1.Repo{{
					Id: defaultRepoID, Guid: defaultRepoGUID, Uri: "test-uri", Password: "secret", Shared: true,
				}},
				Multihost: &v1.Multihost{Identity: identity1, AuthorizedClients: []*v1.Multihost_Peer{peer}},
			})
			snapshot, changes, err := host.configMgr.GetAndSubscribe()
			if err != nil {
				t.Fatal(err)
			}
			defer host.configMgr.OnChange.Unsubscribe(changes)
			if err := host.configMgr.Transform(func(cfg *v1.Config) (*v1.Config, error) {
				if revoked {
					cfg.Multihost.AuthorizedClients = nil
				} else {
					cfg.Multihost.AuthorizedClients[0].Permissions = nil
				}
				return cfg, nil
			}); err != nil {
				t.Fatal(err)
			}
			handler := newSyncHandlerServer(host.manager, &syncConfigSnapshot{config: snapshot}, nil)
			stream := newBidiSyncCommandStream()
			err = handler.sendInitialState(stream, peer)
			if revoked {
				if err == nil {
					t.Fatal("revoked peer was accepted")
				}
				if len(stream.sendChan) != 0 {
					t.Fatal("sent initial state to revoked peer")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			initial := (<-stream.sendChan).GetReceiveConfig().GetConfig()
			if len(initial.GetRepos()) != 0 {
				t.Fatal("sent repository credentials after permission was removed")
			}
			<-stream.sendChan // resource list
			select {
			case item := <-stream.sendChan:
				t.Fatalf("sent shared repository after permission was removed: %v", item)
			default:
			}
		})
	}
}

func TestServerHandshakeUsesCurrentPeerIdentity(t *testing.T) {
	host := newPeerUnderTest(t, &v1.Config{
		Version:  migrations.CurrentVersion,
		Instance: defaultHostID,
		Multihost: &v1.Multihost{Identity: identity1, AuthorizedClients: []*v1.Multihost_Peer{{
			Keyid: identity2.Keyid, InstanceId: "old-client",
		}}},
	})
	stale, err := host.configMgr.Get()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.configMgr.Transform(func(cfg *v1.Config) (*v1.Config, error) {
		cfg.Multihost.AuthorizedClients[0].InstanceId = defaultClientID
		return cfg, nil
	}); err != nil {
		t.Fatal(err)
	}
	localKey, err := cryptoutil.NewPrivateKey(identity1)
	if err != nil {
		t.Fatal(err)
	}
	remoteKey, err := cryptoutil.NewPrivateKey(identity2)
	if err != nil {
		t.Fatal(err)
	}
	transcript := freshTranscript(t)
	packet, err := createHandshakePacket(defaultClientID, remoteKey, "", transcript)
	if err != nil {
		t.Fatal(err)
	}
	oldPacket, err := createHandshakePacket("old-client", remoteKey, "", transcript)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&BackrestSyncHandler{mgr: host.manager}).resolveClientPeer()(oldPacket); err == nil {
		t.Fatal("old instance ID was accepted after authorization changed")
	}
	stream := newBidiSyncCommandStream()
	stream.transcript = transcript
	close(stream.transcriptReady)
	stream.recvChan <- packet
	handler := newSyncHandlerServer(host.manager, &syncConfigSnapshot{config: stale}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runSync(ctx, defaultHostID, localKey, stream, handler,
			stale.Multihost.AuthorizedClients, "", (&BackrestSyncHandler{mgr: host.manager}).resolveClientPeer())
	}()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case <-stream.sendChan: // local handshake
	case err := <-done:
		done <- err
		t.Fatalf("sync failed before local handshake: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case item := <-stream.sendChan:
		if item.GetReceiveConfig() == nil {
			t.Fatalf("expected initial config, got %v", item)
		}
	case err := <-done:
		done <- err
		t.Fatalf("current peer identity rejected: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestSyncManagerAppliesSecretClearedDuringRestart(t *testing.T) {
	mgrConfig := &config.ConfigManager{Store: &config.MemoryStore{Config: &v1.Config{
		Version:  migrations.CurrentVersion,
		Instance: defaultClientID,
		Multihost: &v1.Multihost{Identity: identity2, KnownHosts: []*v1.Multihost_Peer{{
			Keyid: identity1.Keyid, InstanceId: defaultHostID,
			InstanceUrl: "http://127.0.0.1:1", InitialPairingSecret: "reusable-secret",
		}}},
	}}}
	mgr := NewSyncManager(mgrConfig, nil, nil, NewInMemoryPeerStateManager())
	blocked, release := make(chan struct{}), make(chan struct{})
	var blockOnce, releaseOnce sync.Once
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.InfoLevel),
		zap.Hooks(func(entry zapcore.Entry) error {
			if entry.Message == "syncmanager applying new config, waiting for existing sync goroutines to exit" {
				blockOnce.Do(func() {
					close(blocked)
					<-release
				})
			}
			return nil
		}))
	restore := zap.ReplaceGlobals(logger)
	defer restore()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.RunSync(ctx)
	}()
	defer func() {
		releaseOnce.Do(func() { close(release) })
		cancel()
		<-done
	}()
	testutil.Try(t, ctx, func() error {
		if mgr.getSyncConfigSnapshot() == nil {
			return fmt.Errorf("waiting for initial snapshot")
		}
		return nil
	})
	if err := mgrConfig.Transform(func(cfg *v1.Config) (*v1.Config, error) {
		cfg.Multihost.KnownHosts[0].InstanceUrl = "http://127.0.0.1:2"
		return cfg, nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("sync manager did not start processing the config change")
	}
	if err := mgrConfig.Transform(func(cfg *v1.Config) (*v1.Config, error) {
		cfg.Multihost.KnownHosts[0].InitialPairingSecret = ""
		return cfg, nil
	}); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	testutil.Try(t, ctx, func() error {
		snapshot := mgr.getSyncConfigSnapshot()
		if snapshot == nil || snapshot.config.Multihost.KnownHosts[0].InitialPairingSecret != "" {
			return fmt.Errorf("sync manager has not applied the cleared pairing secret")
		}
		return nil
	})
}

func TestPairingTokenExpiredRejected(t *testing.T) {
	testutil.InstallZapLogger(t)

	peerHostAddr := newSyncListener(t)
	peerClientAddr := newSyncListener(t)

	pairingSecret, _ := cryptoutil.GeneratePairingSecret()

	// Host has an expired pairing token.
	peerHostConfig := &v1.Config{
		Version:  migrations.CurrentVersion,
		Instance: defaultHostID,
		Repos:    []*v1.Repo{},
		Multihost: &v1.Multihost{
			Identity:          identity1,
			AuthorizedClients: []*v1.Multihost_Peer{},
			PairingTokens: []*v1.Multihost_PairingToken{
				{
					Secret:        pairingSecret,
					Label:         "expired-token",
					CreatedAtUnix: time.Now().Add(-2 * time.Hour).Unix(),
					ExpiresAtUnix: time.Now().Add(-1 * time.Hour).Unix(), // expired 1 hour ago
					MaxUses:       0,
				},
			},
		},
	}

	peerClientConfig := &v1.Config{
		Version:  migrations.CurrentVersion,
		Instance: defaultClientID,
		Repos:    []*v1.Repo{},
		Multihost: &v1.Multihost{
			Identity: identity2,
			KnownHosts: []*v1.Multihost_Peer{
				{
					Keyid:                identity1.Keyid,
					InstanceId:           defaultHostID,
					InstanceUrl:          fmt.Sprintf("http://%s", peerHostAddr.Addr()),
					InitialPairingSecret: pairingSecret,
				},
			},
		},
	}

	peerHost := newPeerUnderTest(t, peerHostConfig)
	peerClient := newPeerUnderTest(t, peerClientConfig)

	startRunningSyncAPI(t, peerHost, peerHostAddr)
	startRunningSyncAPI(t, peerClient, peerClientAddr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Connection should fail with auth error.
	waitForConnectionState(t, ctx, peerClient, peerClientConfig.Multihost.KnownHosts[0], v1sync.ConnectionState_CONNECTION_STATE_ERROR_AUTH)

	// Host should still have no authorized clients.
	hostConfig, _ := peerHost.configMgr.Get()
	if len(hostConfig.Multihost.AuthorizedClients) != 0 {
		t.Errorf("expected 0 authorized clients, got %d", len(hostConfig.Multihost.AuthorizedClients))
	}
}

func TestPairingTokenMaxUsesEnforced(t *testing.T) {
	testutil.InstallZapLogger(t)

	peerHostAddr := newSyncListener(t)
	peerClient1Addr := newSyncListener(t)

	pairingSecret, _ := cryptoutil.GeneratePairingSecret()

	identity3, _ := cryptoutil.GeneratePrivateKey()

	// Host has a pairing token with max_uses=1.
	peerHostConfig := &v1.Config{
		Version:  migrations.CurrentVersion,
		Instance: defaultHostID,
		Repos:    []*v1.Repo{},
		Multihost: &v1.Multihost{
			Identity:          identity1,
			AuthorizedClients: []*v1.Multihost_Peer{},
			PairingTokens: []*v1.Multihost_PairingToken{
				{
					Secret:        pairingSecret,
					Label:         "single-use",
					CreatedAtUnix: time.Now().Unix(),
					ExpiresAtUnix: time.Now().Add(1 * time.Hour).Unix(),
					MaxUses:       1,
					Uses:          0,
				},
			},
		},
	}

	// First client pairs successfully.
	peerClient1Config := &v1.Config{
		Version:  migrations.CurrentVersion,
		Instance: "client-1",
		Repos:    []*v1.Repo{},
		Multihost: &v1.Multihost{
			Identity: identity2,
			KnownHosts: []*v1.Multihost_Peer{
				{
					Keyid:                identity1.Keyid,
					InstanceId:           defaultHostID,
					InstanceUrl:          fmt.Sprintf("http://%s", peerHostAddr.Addr()),
					InitialPairingSecret: pairingSecret,
				},
			},
		},
	}

	peerHost := newPeerUnderTest(t, peerHostConfig)
	peerClient1 := newPeerUnderTest(t, peerClient1Config)

	var wg sync.WaitGroup
	syncCtx, cancelSync := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancelSync()
		wg.Wait()
	})

	wg.Add(2)
	go func() { defer wg.Done(); runSyncAPIWithCtx(syncCtx, peerHost, peerHostAddr) }()
	go func() {
		defer wg.Done()
		runSyncAPIWithCtx(syncCtx, peerClient1, peerClient1Addr)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tryConnect(t, ctx, peerClient1, peerClient1Config.Multihost.KnownHosts[0])

	// Stop first client, start second client with same pairing secret.
	cancelSync()
	wg.Wait()

	peerClient2Config := &v1.Config{
		Version:  migrations.CurrentVersion,
		Instance: "client-2",
		Repos:    []*v1.Repo{},
		Multihost: &v1.Multihost{
			Identity: identity3,
			KnownHosts: []*v1.Multihost_Peer{
				{
					Keyid:                identity1.Keyid,
					InstanceId:           defaultHostID,
					InstanceUrl:          fmt.Sprintf("http://%s", peerHostAddr.Addr()),
					InitialPairingSecret: pairingSecret,
				},
			},
		},
	}

	peerClient2 := newPeerUnderTest(t, peerClient2Config)

	startRunningSyncAPI(t, peerHost, peerHostAddr)
	startRunningSyncAPI(t, peerClient2, newSyncListener(t))

	// Second client should fail — token is consumed.
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSecond()
	waitForConnectionState(t, secondCtx, peerClient2, peerClient2Config.Multihost.KnownHosts[0], v1sync.ConnectionState_CONNECTION_STATE_ERROR_AUTH)
}

func TestNoPairingSecretRejected(t *testing.T) {
	testutil.InstallZapLogger(t)

	peerHostAddr := newSyncListener(t)
	peerClientAddr := newSyncListener(t)

	// Host has NO pairing tokens and NO authorized clients.
	peerHostConfig := &v1.Config{
		Version:  migrations.CurrentVersion,
		Instance: defaultHostID,
		Repos:    []*v1.Repo{},
		Multihost: &v1.Multihost{
			Identity:          identity1,
			AuthorizedClients: []*v1.Multihost_Peer{},
		},
	}

	// Client tries to connect without any pairing secret.
	peerClientConfig := &v1.Config{
		Version:  migrations.CurrentVersion,
		Instance: defaultClientID,
		Repos:    []*v1.Repo{},
		Multihost: &v1.Multihost{
			Identity: identity2,
			KnownHosts: []*v1.Multihost_Peer{
				{
					Keyid:       identity1.Keyid,
					InstanceId:  defaultHostID,
					InstanceUrl: fmt.Sprintf("http://%s", peerHostAddr.Addr()),
				},
			},
		},
	}

	peerHost := newPeerUnderTest(t, peerHostConfig)
	peerClient := newPeerUnderTest(t, peerClientConfig)

	startRunningSyncAPI(t, peerHost, peerHostAddr)
	startRunningSyncAPI(t, peerClient, peerClientAddr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waitForConnectionState(t, ctx, peerClient, peerClientConfig.Multihost.KnownHosts[0], v1sync.ConnectionState_CONNECTION_STATE_ERROR_AUTH)
}

package tgclient

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gotd/td/crypto"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClientAppliesConnectDefaultsToDials(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		storage := &session.StorageMemory{}
		stored := tg.DCOption{ID: 2, IPAddress: "192.0.2.1", Port: 443}
		key := crypto.Key{1}
		keyID := key.ID()
		require.NoError(t, (&session.Loader{Storage: storage}).Save(ctx, &session.Data{
			DC: 2, Config: session.Config{DCOptions: []tg.DCOption{stored}},
			AuthKey: key[:], AuthKeyID: keyID[:],
		}))

		type dial struct {
			addr        string
			timeout     time.Duration
			hasDeadline bool
		}
		dials := make(chan dial, len(dcs.Prod().Options)+1)
		resolver := dcs.Plain(dcs.PlainOptions{
			Dial: func(ctx context.Context, _, addr string) (net.Conn, error) {
				deadline, ok := ctx.Deadline()
				dials <- dial{addr: addr, timeout: time.Until(deadline), hasDeadline: ok}
				<-ctx.Done()
				return nil, ctx.Err()
			},
		})
		client := newClient(ctx, &Config{APIID: 1, APIHash: "hash"}, storage, slog.New(slog.DiscardHandler), telegram.Options{Resolver: resolver})
		done := make(chan error, 1)
		go func() {
			done <- client.Run(ctx, func(context.Context) error {
				return errors.New("a blocked dial must not reach readiness")
			})
		}()
		synctest.Wait()
		cancel()
		require.NoError(t, <-done)
		synctest.Wait()

		close(dials)
		var addresses []string
		for attempt := range dials {
			addresses = append(addresses, attempt.addr)
			assert.True(t, attempt.hasDeadline, "dial %s must have a deadline", attempt.addr)
			assert.Equal(t, 10*time.Second, attempt.timeout, "dial %s must use the configured startup budget", attempt.addr)
		}
		assert.Contains(t, addresses, "192.0.2.1:443", "the client must actually dial the saved DC")
		builtIn := dcs.FindPrimaryDCs(dcs.Prod().Options, 2, false)[0]
		assert.Contains(t, addresses, net.JoinHostPort(builtIn.IPAddress, strconv.Itoa(builtIn.Port)), "the built-in DC must remain a dial candidate")
	})
}

func TestWithConnectDefaultsDialsStoredDCsFirst(t *testing.T) {
	ctx := context.Background()
	storage := &session.StorageMemory{}
	stored := tg.DCOption{ID: 2, IPAddress: "149.154.167.51", Port: 443}
	err := (&session.Loader{Storage: storage}).Save(ctx, &session.Data{
		DC:      2,
		Config:  session.Config{DCOptions: []tg.DCOption{stored}},
		AuthKey: make([]byte, 256),
	})
	if err != nil {
		t.Fatalf("saving session: %v", err)
	}

	opts := withConnectDefaults(ctx, storage, telegram.Options{})

	if opts.DialTimeout != dialTimeout {
		t.Errorf("DialTimeout = %s, want %s", opts.DialTimeout, dialTimeout)
	}
	got := dcs.FindPrimaryDCs(opts.DCList.Options, 2, false)
	if len(got) < 2 || got[0].IPAddress != stored.IPAddress {
		t.Fatalf("DC 2 candidates = %+v, want the stored address first and the built-in ones after it", got)
	}
	builtIn := dcs.FindPrimaryDCs(dcs.Prod().Options, 2, false)[0].IPAddress
	found := false
	for _, o := range got {
		found = found || o.IPAddress == builtIn
	}
	if !found {
		t.Errorf("DC 2 candidates = %+v, want the built-in %s kept as a fallback", got, builtIn)
	}
}

func TestWithConnectDefaultsWithoutSessionKeepsGotdDefaults(t *testing.T) {
	opts := withConnectDefaults(context.Background(), &session.StorageMemory{}, telegram.Options{})
	if !opts.DCList.Zero() {
		t.Errorf("DCList = %+v, want it left to gotd when there is no session", opts.DCList)
	}
}

func TestWithConnectDefaultsKeepsExplicitList(t *testing.T) {
	list := dcs.List{Options: []tg.DCOption{{ID: 1, IPAddress: "127.0.0.1", Port: 1}}}
	opts := withConnectDefaults(context.Background(), &session.StorageMemory{}, telegram.Options{DCList: list})
	if len(opts.DCList.Options) != 1 || opts.DCList.Options[0].IPAddress != "127.0.0.1" {
		t.Errorf("DCList = %+v, want the caller's list untouched", opts.DCList)
	}
}

func TestMergeDCOptionsDropsDuplicates(t *testing.T) {
	a := tg.DCOption{ID: 2, IPAddress: "149.154.167.41", Port: 443}
	media := tg.DCOption{ID: 2, IPAddress: "149.154.167.41", Port: 443, MediaOnly: true}
	b := tg.DCOption{ID: 2, IPAddress: "149.154.167.51", Port: 443}

	got := mergeDCOptions([]tg.DCOption{b, a}, []tg.DCOption{a, media})

	if len(got) != 3 || got[0].IPAddress != b.IPAddress || got[1].IPAddress != a.IPAddress || got[1].MediaOnly || !got[2].MediaOnly {
		t.Errorf("merged = %+v, want [b a media]", got)
	}
}

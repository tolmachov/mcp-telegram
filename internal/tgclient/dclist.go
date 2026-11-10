package tgclient

import (
	"context"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
)

// dialTimeout bounds one connection attempt. gotd's own default (35s) is longer
// than startClientTimeout, so an address that silently drops packets would use
// up the whole startup budget without a single failure being reported.
const dialTimeout = 10 * time.Second

// withConnectDefaults fills in where a client dials when opts leaves it to gotd.
//
// gotd restores the auth key from the stored session but not the DC list
// Telegram sent with it: every start dials the addresses compiled into gotd
// (dcs.Prod), which list a single IPv4 address for most DCs. When that one
// address is unreachable from the user's network the client hangs on it,
// even though Telegram's own current list — saved in the session on every
// connect — names others that answer. The stored list goes first and the
// built-in one stays as a fallback; gotd dials the candidates in parallel
// and keeps the first that answers.
//
// Options that already name their DCs (tests run an in-process cluster) are
// left alone.
func withConnectDefaults(ctx context.Context, storage session.Storage, opts telegram.Options) telegram.Options {
	if opts.DialTimeout == 0 {
		opts.DialTimeout = dialTimeout
	}
	if !opts.DCList.Zero() || storage == nil {
		return opts
	}
	prod := dcs.Prod()
	data, err := (&session.Loader{Storage: storage}).Load(ctx)
	if err != nil || len(data.Config.DCOptions) == 0 {
		// No session yet (login) or an unreadable one: gotd reports the
		// latter itself when it restores the connection.
		return opts
	}
	opts.DCList = dcs.List{
		Options: mergeDCOptions(data.Config.DCOptions, prod.Options),
		Domains: prod.Domains,
	}
	return opts
}

// mergeDCOptions returns primary followed by the entries of fallback it does
// not already contain.
func mergeDCOptions(primary, fallback []tg.DCOption) []tg.DCOption {
	type key struct {
		id                     int
		addr                   string
		port                   int
		ipv6, media, cdn, obfs bool
	}
	seen := make(map[key]bool, len(primary)+len(fallback))
	out := make([]tg.DCOption, 0, len(primary)+len(fallback))
	for _, list := range [][]tg.DCOption{primary, fallback} {
		for _, o := range list {
			k := key{o.ID, o.IPAddress, o.Port, o.Ipv6, o.MediaOnly, o.CDN, o.TCPObfuscatedOnly}
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, o)
		}
	}
	return out
}

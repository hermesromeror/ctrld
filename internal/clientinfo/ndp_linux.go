package clientinfo

import (
	"context"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/Control-D-Inc/ctrld"
)

const (
	ndpNetlinkRcvBuf     = 4 * 1024 * 1024
	ndpChannelBufSize    = 1024
	ndpResubscribeDelay  = 5 * time.Second
	ndpReasonChanClosed  = "neighbor channel closed"
	ndpReasonContextDone = "context done"
)

// scan populates NDP table using information from system mappings.
func (nd *ndpDiscover) scan() {
	neighs, err := netlink.NeighList(0, netlink.FAMILY_V6)
	if err != nil {
		ctrld.ProxyLogger.Load().Warn().Err(err).Msg("could not get neigh list")
		return
	}

	for _, n := range neighs {
		// Skipping non-reachable neighbors.
		if n.State&netlink.NUD_REACHABLE == 0 {
			continue
		}
		ip := n.IP.String()
		mac := n.HardwareAddr.String()
		nd.saveInfo(ip, mac)
	}
}

func (nd *ndpDiscover) neighSubscribe(ch chan<- netlink.NeighUpdate, done <-chan struct{}) error {
	opts := netlink.NeighSubscribeOptions{
		ErrorCallback: func(err error) {
			ctrld.ProxyLogger.Load().Warn().Err(err).Msg("NDP neighbor subscription error")
		},
		ReceiveBufferSize:      ndpNetlinkRcvBuf,
		ReceiveBufferForceSize: true,
	}
	if err := netlink.NeighSubscribeWithOptions(ch, done, opts); err != nil {
		ctrld.ProxyLogger.Load().Warn().Err(err).Msg("NDP neighbor subscribe with SO_RCVBUFFORCE failed, retrying with SO_RCVBUF")
		opts.ReceiveBufferForceSize = false
		return netlink.NeighSubscribeWithOptions(ch, done, opts)
	}
	return nil
}

// subscribe watches NDP table changes and update new information to local table.
func (nd *ndpDiscover) subscribe(ctx context.Context) {
	for attempt := 1; ; attempt++ {
		reason := nd.subscribeOnce(ctx, attempt)
		if reason != ndpReasonChanClosed {
			ctrld.ProxyLogger.Load().Warn().Msgf("NDP neighbor subscription #%d ended: %s", attempt, reason)
			return
		}
		ctrld.ProxyLogger.Load().Warn().Msgf("NDP neighbor subscription #%d ended (%s), re-subscribing in %s", attempt, reason, ndpResubscribeDelay)
		timer := time.NewTimer(ndpResubscribeDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		nd.scan()
	}
}

func (nd *ndpDiscover) subscribeOnce(ctx context.Context, attempt int) string {
	ch := make(chan netlink.NeighUpdate, ndpChannelBufSize)
	done := make(chan struct{})
	defer close(done)
	if err := nd.neighSubscribe(ch, done); err != nil {
		ctrld.ProxyLogger.Load().Err(err).Msg("could not perform neighbor subscribing")
		return "subscribe error"
	}
	ctrld.ProxyLogger.Load().Info().Msgf("NDP neighbor subscription #%d started (rcvbuf requested %d bytes)", attempt, ndpNetlinkRcvBuf)
	for {
		select {
		case <-ctx.Done():
			return ndpReasonContextDone
		case nu, ok := <-ch:
			if !ok {
				return ndpReasonChanClosed
			}
			if nu.Family != netlink.FAMILY_V6 {
				continue
			}
			ip := normalizeIP(nu.IP.String())
			if nu.Type == unix.RTM_DELNEIGH {
				ctrld.ProxyLogger.Load().Debug().Msgf("removing NDP neighbor: %s", ip)
				nd.mac.Delete(ip)
				continue
			}
			mac := nu.HardwareAddr.String()
			switch nu.State {
			case netlink.NUD_REACHABLE:
				nd.saveInfo(ip, mac)
			case netlink.NUD_FAILED:
				ctrld.ProxyLogger.Load().Debug().Msgf("removing NDP neighbor with failed state: %s", ip)
				nd.mac.Delete(ip)
			}
		}
	}
}

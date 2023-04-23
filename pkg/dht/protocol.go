package dht

import (
	"fmt"
	"sync"

	kaddht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/dennis-tra/pcp/internal/wrap"
	"github.com/dennis-tra/pcp/pkg/discovery"
	"github.com/dennis-tra/pcp/pkg/service"
)

// These wrapped top level functions are here for testing purposes.
var (
	wrapDHT   wrap.DHTer   = wrap.DHT{}
	wraptime  wrap.Timer   = wrap.Time{}
	wrapmanet wrap.Maneter = wrap.Manet{}
)

// protocol encapsulates the logic for discovering peers
// through providing it in the IPFS DHT.
type protocol struct {
	host.Host

	// Service holds an abstraction of a long-running
	// service that is started and stopped externally.
	service.Service
	dht wrap.IpfsDHT
	did discovery.ID
}

func newProtocol(h host.Host, dht wrap.IpfsDHT) *protocol {
	p := &protocol{
		Host:    h,
		dht:     dht,
		Service: service.New("DHT"),
		did:     discovery.ID{},
	}

	return p
}

// bootstrap connects to a set of bootstrap nodes to connect
// to the DHT.
func (p *protocol) bootstrap() error {
	peers := kaddht.GetDefaultBootstrapPeerAddrInfos()
	peerCount := len(peers)
	if peerCount == 0 {
		return fmt.Errorf("no bootstrap peers configured")
	}

	// Asynchronously connect to all bootstrap peers and send
	// potential errors to a channel. This channel is used
	// to capture the errors and check if we have established
	// enough connections. An error group (errgroup) cannot
	// be used here as it exits as soon as an error is thrown
	// in one of the Go-Routines.
	var wg sync.WaitGroup
	errChan := make(chan error, peerCount)
	for _, bp := range peers {
		wg.Add(1)
		go func(pi peer.AddrInfo) {
			defer wg.Done()
			errChan <- p.Connect(p.ServiceContext(), pi)
		}(bp)
	}

	// Close error channel after all connection attempts are done
	// to signal the for-loop below to stop.
	go func() {
		wg.Wait()
		close(errChan)
	}()

	// Reading the error channel and collect errors.
	errs := ErrConnThresholdNotReached{BootstrapErrs: []error{}}
	for {
		err, ok := <-errChan
		if !ok {
			// channel was closed.
			break
		} else if err != nil {
			errs.BootstrapErrs = append(errs.BootstrapErrs, err)
		}
	}

	// If we could not establish enough connections return an error
	if peerCount-len(errs.BootstrapErrs) < ConnThreshold {
		select {
		case <-p.ServiceContext().Done():
			return p.ServiceContext().Err()
		default:
			return errs
		}
	}

	return nil
}

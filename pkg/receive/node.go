package receive

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/libp2p/go-libp2p/core/network"

	"github.com/dennis-tra/pcp/pkg/discovery"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/urfave/cli/v2"

	"github.com/dennis-tra/pcp/internal/format"
	"github.com/dennis-tra/pcp/internal/log"
	"github.com/dennis-tra/pcp/pkg/dht"
	"github.com/dennis-tra/pcp/pkg/mdns"
	pcpnode "github.com/dennis-tra/pcp/pkg/node"
	p2p "github.com/dennis-tra/pcp/pkg/pb"
)

type PeerState uint8

const (
	NotConnected PeerState = iota
	Connecting
	Connected
	FailedConnecting
	FailedAuthentication
)

type Node struct {
	*pcpnode.Node

	// mDNS discovery implementations
	mdnsDiscoverer       *mdns.Discoverer
	mdnsDiscovererOffset *mdns.Discoverer

	// DHT discovery implementations
	dhtDiscoverer       *dht.Discoverer
	dhtDiscovererOffset *dht.Discoverer

	autoAccept bool
	peerStates sync.Map

	// a logging service which updates the terminal with the current state
	statusLogger *statusLogger

	netNotifeeLk sync.RWMutex
	netNotifee   *network.NotifyBundle
}

func InitNode(c *cli.Context, words []string) (*Node, error) {
	h, err := pcpnode.New(c, words)
	if err != nil {
		return nil, err
	}

	node := &Node{
		Node:       h,
		autoAccept: c.Bool("auto-accept"),
		peerStates: sync.Map{},
	}

	node.mdnsDiscoverer = mdns.NewDiscoverer(node, node)
	node.mdnsDiscovererOffset = mdns.NewDiscoverer(node, node).SetOffset(-discovery.TruncateDuration)
	node.dhtDiscoverer = dht.NewDiscoverer(node, node.DHT, node)
	node.dhtDiscovererOffset = dht.NewDiscoverer(node, node.DHT, node).SetOffset(-discovery.TruncateDuration)
	node.statusLogger = newStatusLogger(node)

	node.RegisterPushRequestHandler(node)

	// start logging the current status to the console
	if !c.Bool("debug") {
		go node.statusLogger.startLogging()
	}

	// stop the process if all discoverers error out
	go node.watchDiscoverErrors()

	return node, nil
}

func (n *Node) Shutdown() {
	go func() {
		<-n.SigShutdown()
		n.stopDiscovering()
		n.UnregisterTransferHandler()
		n.statusLogger.Shutdown()

		// TODO: properly closing the host can take up to 1 minute
		//if err := n.Host.Close(); err != nil {
		//	log.Warningln("error stopping libp2p node:", err)
		//}

		n.ServiceStopped()
	}()
	n.Service.Shutdown()
}

func (n *Node) StartDiscoveringMDNS() {
	n.SetState(pcpnode.Roaming)
	go n.mdnsDiscoverer.Discover(n.ChanID)
	go n.mdnsDiscovererOffset.Discover(n.ChanID)
}

func (n *Node) StartDiscoveringDHT() {
	n.SetState(pcpnode.Roaming)
	go n.dhtDiscoverer.Discover(n.ChanID)
	go n.dhtDiscovererOffset.Discover(n.ChanID)
}

func (n *Node) stopDiscovering() {
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		n.mdnsDiscoverer.Shutdown()
		wg.Done()
	}()

	wg.Add(1)
	go func() {
		n.mdnsDiscovererOffset.Shutdown()
		wg.Done()
	}()

	wg.Add(1)
	go func() {
		n.dhtDiscoverer.Shutdown()
		wg.Done()
	}()

	wg.Add(1)
	go func() {
		n.dhtDiscovererOffset.Shutdown()
		wg.Done()
	}()

	wg.Wait()
}

func (n *Node) watchDiscoverErrors() {
	for {
		select {
		case <-n.SigShutdown():
			return
		case <-n.mdnsDiscoverer.SigDone():
		case <-n.mdnsDiscovererOffset.SigDone():
		case <-n.dhtDiscoverer.SigDone():
		case <-n.dhtDiscovererOffset.SigDone():
		}
		mdnsState := n.mdnsDiscoverer.State()
		mdnsOffsetState := n.mdnsDiscovererOffset.State()
		dhtState := n.dhtDiscoverer.State()
		dhtOffsetState := n.dhtDiscovererOffset.State()

		// if all discoverers errored out, stop the process
		if mdnsState.Stage == mdns.StageError && mdnsOffsetState.Stage == mdns.StageError &&
			dhtState.Stage == dht.StageError && dhtOffsetState.Stage == dht.StageError {
			n.Shutdown()
			return
		}

		// if all discoverers reached a termination stage (e.g., both were stopped or one was stopped, the other
		// experienced an error), we have found and successfully connected to a peer. This means, all good - just
		// stop this go routine.
		if mdnsState.Stage.IsTermination() && mdnsOffsetState.Stage.IsTermination() &&
			dhtState.Stage.IsTermination() && dhtOffsetState.Stage.IsTermination() {
			return
		}
	}
}

// HandlePeerFound is called async from the discoverers. It's okay to have long-running tasks here.
func (n *Node) HandlePeerFound(pi peer.AddrInfo) {
	if n.GetState() != pcpnode.Roaming {
		log.Debugln("Received a peer from the discoverer although we're not discovering")
		return
	}

	// Add discovered peer to the hole punch allow list to track the
	// hole punch state of that particular peer as soon as we try to
	// connect to them.
	n.AddToHolePunchAllowList(pi.ID)

	// Check if we have already seen the peer and exit early to not connect again.
	peerState, _ := n.peerStates.LoadOrStore(pi.ID, NotConnected)
	switch peerState.(PeerState) {
	case NotConnected:
	case Connecting:
		log.Debugln("Skipping node as we're already trying to connect", pi.ID)
		return
	case FailedConnecting:
		// TODO: Check if multiaddrs have changed and only connect if that's the case
		log.Debugln("We tried to connect previously but couldn't establish a connection, try again", pi.ID)
	case FailedAuthentication:
		log.Debugln("We tried to connect previously but the node didn't pass authentication  -> skipping", pi.ID)
		return
	}

	log.Debugln("Connecting to peer:", pi.ID)
	n.peerStates.Store(pi.ID, Connecting)
	if err := n.Connect(n.ServiceContext(), pi); err != nil {
		log.Debugln("Error connecting to peer:", pi.ID, err)
		n.peerStates.Store(pi.ID, FailedConnecting)
		return
	}

	n.DebugLogAuthenticatedPeer(pi.ID)

	// Negotiate PAKE
	if _, err := n.StartKeyExchange(n.ServiceContext(), pi.ID); err != nil {
		log.Errorln("Peer didn't pass authentication:", err)
		n.peerStates.Store(pi.ID, FailedAuthentication)
		return
	}
	n.peerStates.Store(pi.ID, Connected)

	// We're authenticated so can initiate a transfer
	if n.GetState() == pcpnode.Connected {
		log.Debugln("already connected and authenticated with another node")
		return
	}

	// can't stopNotify in Shutdown -> deadlock - but don't understand why
	//n.netNotifeeLk.Lock()
	//n.netNotifee = &network.NotifyBundle{
	//	DisconnectedF: func(net network.Network, conn network.Conn) {
	//		if conn.RemotePeer() != pi.ID {
	//			return
	//		}
	//
	//		if net.Connectedness(conn.RemotePeer()) == network.Connected {
	//			return
	//		}
	//
	//		log.Warningln("Lost connection to remote peer - shutting down")
	//
	//		n.Shutdown()
	//	},
	//}
	//n.netNotifeeLk.Unlock()
	//
	//n.Network().Notify(n.netNotifee)

	// between registering to be notified until here we could have been disconnected,
	// so check again here.
	if n.Network().Connectedness(pi.ID) == network.NotConnected {
		n.Shutdown()
		return
	}

	n.SetState(pcpnode.Connected)

	// Stop the discovering process as we have found the valid peer
	n.stopDiscovering()

	// wait until the hole punch has succeeded
	err := n.WaitForDirectConn(pi.ID)
	if err != nil {
		n.statusLogger.Shutdown()
		n.Shutdown()
		log.Infoln("Hole punching failed:", err)
		return
	}
	n.statusLogger.Shutdown()

	// make sure we don't open the new transfer-stream on the relayed connection.
	// libp2p claims to not do that, but I have observed strange connection resets.
	n.CloseRelayedConnections(pi.ID)
}

func (n *Node) HandlePushRequest(pr *p2p.PushRequest) (bool, error) {
	if n.autoAccept {
		return n.handleAccept(pr)
	}

	obj := "File"
	if pr.IsDir {
		obj = "Directory"
	}
	log.Infof("%s: %s (%s)\n", obj, pr.Name, format.Bytes(pr.Size))
	for {
		log.Infof("Do you want to receive this %s? [y,n,i,?] ", strings.ToLower(obj))
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return true, fmt.Errorf("failed reading from stdin: %w", scanner.Err())
		}

		// sanitize user input
		input := strings.ToLower(strings.TrimSpace(scanner.Text()))

		// Empty input, user just pressed enter => do nothing and prompt again
		if input == "" {
			continue
		}

		// Print the help text and prompt again
		if input == "?" {
			help()
			continue
		}

		// Print information about the send request
		if input == "i" {
			printInformation(pr)
			continue
		}

		// Accept the file transfer
		if input == "y" {
			return n.handleAccept(pr)
		}

		// Reject the file transfer
		if input == "n" {
			go n.Shutdown()
			return false, nil
		}

		log.Infoln("Invalid input")
	}
}

// handleAccept handles the case when the user accepted the transfer or provided
// the corresponding command line flag.
func (n *Node) handleAccept(pr *p2p.PushRequest) (bool, error) {
	done := n.TransferFinishHandler(pr.Size)
	th, err := NewTransferHandler(pr.Name, done)
	if err != nil {
		return true, err
	}
	n.RegisterTransferHandler(th)
	return true, nil
}

func (n *Node) TransferFinishHandler(size int64) chan int64 {
	done := make(chan int64)
	go func() {
		var received int64
		select {
		case <-n.SigShutdown():
			return
		case received = <-done:
		}

		if received == size {
			log.Infoln("Successfully received file/directory!")
		} else {
			log.Infof("WARNING: Only received %d of %d bytes!\n", received, size)
		}

		n.Shutdown()
	}()
	return done
}

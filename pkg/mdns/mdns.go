package mdns

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/sirupsen/logrus"

	"github.com/dennis-tra/pcp/pkg/discovery"
	"github.com/dennis-tra/pcp/pkg/tui"
)

var log = logrus.WithField("comp", "mdns")

// Model encapsulates the logic for roaming
// via multicast DNS in the local network.
type Model struct {
	host   host.Host
	chanID int

	services map[time.Duration]mdns.Service

	sender  tea.Sender
	spinner spinner.Model

	State State
	Err   error

	// for testing
	newMdnsService func(host.Host, string, mdns.Notifee) mdns.Service
}

func New(h host.Host, sender tea.Sender, chanID int) *Model {
	m := &Model{
		host:    h,
		chanID:  chanID,
		sender:  sender,
		spinner: spinner.New(spinner.WithSpinner(spinner.Dot)),
		newMdnsService: func(host host.Host, serviceName string, notifee mdns.Notifee) mdns.Service {
			return mdns.NewMdnsService(host, serviceName, notifee)
		},
	}

	m.reset()

	return m
}

func (m *Model) Init() tea.Cmd {
	log.Traceln("tea init")
	return m.spinner.Tick
}

func (m *Model) Update(msg tea.Msg) (*Model, tea.Cmd) {
	m.logEntry().WithField("type", fmt.Sprintf("%T", msg)).Tracef("handle message: %T\n", msg)

	var (
		cmd  tea.Cmd
		cmds []tea.Cmd
	)

	switch msg := msg.(type) {
	case updateMsg:
		if m.State != StateStarted {
			log.Fatal("mDNS service not running")
			return m, nil
		}

		svc, found := m.services[msg.offset]
		if !found {
			return m, nil
		}

		logEntry := m.logEntry().WithField("offset", msg.offset)
		logEntry.Traceln("Updating mDNS service")

		if err := svc.Close(); err != nil {
			log.WithError(err).Warningln("Couldn't close mDNS service")
		}

		svc, err := m.newService(msg.offset)
		if err != nil {
			m.reset()
			m.State = StateError
			m.Err = fmt.Errorf("start mdns service offset: %w", err)
			return m, nil
		}
		m.services[msg.offset] = svc

		cmds = append(cmds, m.wait(msg.offset))

	case stopMsg:
		if m.State != StateStarted {
			return m, nil
		}
		m.logEntry().WithError(msg.reason).Infoln("Stopping mDNS service")

		m, cmd = m.StopWithReason(msg.reason)
		cmds = append(cmds, cmd)
	}

	m.spinner, cmd = m.spinner.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

func (m *Model) View() string {
	switch m.State {
	case StateIdle:
		return tui.Faint.Render("not started")
	case StateStarted:
		return tui.Green.Render("ready")
	case StateStopped:
		if errors.Is(m.Err, context.Canceled) {
			return tui.Faint.Render("cancelled")
		} else {
			return tui.Green.Render("stopped")
		}
	case StateError:
		return tui.Red.Render("failed")
	default:
		return tui.Red.Render("unknown state")
	}
}

func (m *Model) logEntry() *logrus.Entry {
	return log.WithFields(logrus.Fields{
		"chanID": m.chanID,
		"state":  m.State.String(),
	})
}

func (m *Model) reset() {
	// close already started services
	for _, s := range m.services {
		if err := s.Close(); err != nil {
			log.WithError(err).Warnln("Failed closing mDNS service")
		}
	}

	m.services = map[time.Duration]mdns.Service{}
	m.State = StateIdle
	m.Err = nil
}

func (m *Model) newService(offset time.Duration) (mdns.Service, error) {
	did := discovery.NewID(offset).DiscoveryID(m.chanID)
	logEntry := m.logEntry().
		WithField("did", did).
		WithField("offset", offset.String())
	logEntry.Infoln("Starting mDNS service")

	svc := m.newMdnsService(m.host, did, m)
	if err := svc.Start(); err != nil {
		logEntry.WithError(err).Warnln("Failed starting mDNS service")
		return nil, fmt.Errorf("start mdns service offset: %w", err)
	}

	return svc, nil
}

func (m *Model) HandlePeerFound(pi peer.AddrInfo) {
	logEntry := log.WithFields(logrus.Fields{
		"comp":   "mdns",
		"peerID": pi.ID.String()[:16],
	})

	if pi.ID == m.host.ID() {
		logEntry.Traceln("Found ourself")
		return
	}

	pi.Addrs = onlyPrivate(pi.Addrs)
	if len(pi.Addrs) == 0 {
		logEntry.Debugln("Peer has no private addresses")
		return
	}

	logEntry.Infoln("Found peer via mDNS!")
	m.sender.Send(PeerMsg(pi))
}

// Filter out addresses that are public - only allow private ones.
func onlyPrivate(addrs []ma.Multiaddr) []ma.Multiaddr {
	var routable []ma.Multiaddr
	for _, addr := range addrs {
		if manet.IsPrivateAddr(addr) {
			routable = append(routable, addr)
			log.Debugf("\tprivate - %s\n", addr.String())
		} else {
			log.Debugf("\tpublic - %s\n", addr.String())
		}
	}
	return routable
}

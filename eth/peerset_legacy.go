// Copyright 2020 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"fmt"
	"github.com/ethereum/go-ethereum/eth/protocols/mir"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/p2p"
)

// peerSet represents the collection of active peers currently participating in
// the `eth` protocol, with or without the `snap` extension.
type legacyPeerSet struct {
	peers     map[string]*mirPeer // Peers connected on the `eth` protocol
	snapPeers int                 // Number of `snap` compatible peers for connection prioritization

	snapWait map[string]chan *snap.Peer // Peers connected on `eth` waiting for their snap extension
	snapPend map[string]*snap.Peer      // Peers connected on the `snap` protocol, but not yet on `eth`

	lock   sync.RWMutex
	closed bool
	quitCh chan struct{} // Quit channel to signal termination
}

// newPeerSet creates a new peer set to track the active participants.
func newLegacyPeerSet() *legacyPeerSet {
	return &legacyPeerSet{
		peers:    make(map[string]*mirPeer),
		snapWait: make(map[string]chan *snap.Peer),
		snapPend: make(map[string]*snap.Peer),
		quitCh:   make(chan struct{}),
	}
}

// registerSnapExtension unblocks an already connected `eth` peer waiting for its
// `snap` extension, or if no such peer exists, tracks the extension for the time
// being until the `eth` main protocol starts looking for it.
func (ps *legacyPeerSet) registerSnapExtension(peer *snap.Peer) error {
	// Reject the peer if it advertises `snap` without `eth` as `snap` is only a
	// satellite protocol meaningful with the chain selection of `eth`
	if !peer.RunningCap(mir.ProtocolName, mir.ProtocolVersions) {
		return fmt.Errorf("%w: have %v", errSnapWithoutEth, peer.Caps())
	}
	// Ensure nobody can double connect
	ps.lock.Lock()
	defer ps.lock.Unlock()

	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		return errPeerAlreadyRegistered // avoid connections with the same id as existing ones
	}
	if _, ok := ps.snapPend[id]; ok {
		return errPeerAlreadyRegistered // avoid connections with the same id as pending ones
	}
	// Inject the peer into an `eth` counterpart is available, otherwise save for later
	if wait, ok := ps.snapWait[id]; ok {
		delete(ps.snapWait, id)
		wait <- peer
		return nil
	}
	ps.snapPend[id] = peer
	return nil
}

// waitSnapExtension blocks until all satellite protocols are connected and tracked
// by the peerset.
func (ps *legacyPeerSet) waitSnapExtension(peer *mir.Peer) (*snap.Peer, error) {
	// If the peer does not support a compatible `snap`, don't wait
	if !peer.RunningCap(snap.ProtocolName, snap.ProtocolVersions) {
		return nil, nil
	}
	// Ensure nobody can double connect
	ps.lock.Lock()

	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		ps.lock.Unlock()
		return nil, errPeerAlreadyRegistered // avoid connections with the same id as existing ones
	}
	if _, ok := ps.snapWait[id]; ok {
		ps.lock.Unlock()
		return nil, errPeerAlreadyRegistered // avoid connections with the same id as pending ones
	}
	// If `snap` already connected, retrieve the peer from the pending set
	if snap, ok := ps.snapPend[id]; ok {
		delete(ps.snapPend, id)

		ps.lock.Unlock()
		return snap, nil
	}
	// Otherwise wait for `snap` to connect concurrently
	wait := make(chan *snap.Peer)
	ps.snapWait[id] = wait
	ps.lock.Unlock()

	select {
	case p := <-wait:
		return p, nil
	case <-ps.quitCh:
		ps.lock.Lock()
		delete(ps.snapWait, id)
		ps.lock.Unlock()
		return nil, errPeerSetClosed
	}
}

// registerPeer injects a new `eth` peer into the working set, or returns an error
// if the peer is already known.
func (ps *legacyPeerSet) registerPeer(peer *mir.Peer, ext *snap.Peer) error {
	// Start tracking the new peer
	ps.lock.Lock()
	defer ps.lock.Unlock()

	if ps.closed {
		return errPeerSetClosed
	}
	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		return errPeerAlreadyRegistered
	}
	eth := &mirPeer{
		Peer: peer,
	}
	if ext != nil {
		eth.snapExt = &snapPeer{ext}
		ps.snapPeers++
	}
	ps.peers[id] = eth
	return nil
}

// unregisterPeer removes a remote peer from the active set, disabling any further
// actions to/from that particular entity.
func (ps *legacyPeerSet) unregisterPeer(id string) error {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	peer, ok := ps.peers[id]
	if !ok {
		return errPeerNotRegistered
	}
	delete(ps.peers, id)
	if peer.snapExt != nil {
		ps.snapPeers--
	}
	return nil
}

// peer retrieves the registered peer with the given id.
func (ps *legacyPeerSet) peer(id string) *mirPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return ps.peers[id]
}

func (ps *legacyPeerSet) peersWithoutBlock(hash common.Hash) []*mirPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*mirPeer, 0, len(ps.peers))
	for _, p := range ps.peers {
		if !p.KnownBlock(hash) {
			list = append(list, p)
		}
	}
	return list
}

// peersWithoutTransaction retrieves a list of peers that do not have a given
// transaction in their set of known hashes.
func (ps *legacyPeerSet) peersWithoutTransaction(hash common.Hash) []*mirPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*mirPeer, 0, len(ps.peers))
	for _, p := range ps.peers {
		if !p.KnownTransaction(hash) {
			list = append(list, p)
		}
	}
	return list
}

// len returns if the current number of `eth` peers in the set. Since the `snap`
// peers are tied to the existence of an `eth` connection, that will always be a
// subset of `eth`.
func (ps *legacyPeerSet) len() int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return len(ps.peers)
}

// snapLen returns if the current number of `snap` peers in the set.
func (ps *legacyPeerSet) snapLen() int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return ps.snapPeers
}

// close disconnects all peers.
func (ps *legacyPeerSet) close() {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	for _, p := range ps.peers {
		p.Disconnect(p2p.DiscQuitting)
	}
	if !ps.closed {
		close(ps.quitCh)
	}
	ps.closed = true
}

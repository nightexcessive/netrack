package models

import (
	"math"
	"math/rand"
	"time"

	"appengine"
	"appengine/datastore"
	"appengine/memcache"
	"github.com/gaego/ds"
	dserrors "github.com/gaego/ds/errors"

	"app"
)

type Peer struct {
	Key     *datastore.Key `datastore:"-" bencode:"-"`
	Hash    string         `bencode:"-"`
	ID      string         `datastore:",noindex" bencode:"peer id"`
	IP      string         `datastore:",noindex" bencode:"ip"`
	Port    int32          `datastore:",noindex" bencode:"port"`
	Expires time.Time      `bencode:"-"`
}

const (
	// How long we allow for a peer to contact the tracker again. This will be given to the client as the 'interval.'
	PeerExpireTime time.Duration = 60 * time.Minute
	// How often we update the expire time. This is to cut down on the amount of datastore writes that occur.
	PeerRefreshTime time.Duration = 50 * time.Minute
	// The amount of time at which we refresh the peer.
	peerRefreshTimeLeft time.Duration = PeerExpireTime - PeerRefreshTime
	// The grace period for contacting the tracker. This will be added to the expire time to allow a small bit longer for the tracker to be contacted.
	PeerExpireGrace time.Duration = 5 * time.Minute
)

func getPeerKey(c appengine.Context, hash string, peerID string) *datastore.Key {
	return datastore.NewKey(c, "Peer", hash+":"+peerID, 0, nil)
}

func getPeerMemcacheKey(key *datastore.Key) string {
	return "peers:" + key.String()
}

func getHashKey(c appengine.Context, hash string) string {
	return "peers:" + hash
}

type removePeerFuture struct {
	ctx           appengine.Context
	resultChannel chan error

	hash   string
	peerID string
}

func (r *removePeerFuture) GetResult() (interface{}, error) {
	return nil, app.ResolveFutureError(r.resultChannel)
}

func (r *removePeerFuture) Wait() {
	// Iterate over the result channel until it's closed.
	for ok := true; ok; _, ok = <-r.resultChannel {
		continue
	}
}

func RemovePeer(c appengine.Context, hash string, peerID string) app.Future {
	future := &removePeerFuture{
		ctx:           c,
		resultChannel: make(chan error),

		hash:   hash,
		peerID: peerID,
	}

	go actualRemovePeer(future)

	return future
}

func actualRemovePeer(future *removePeerFuture) {
	defer close(future.resultChannel)

	future.ctx.Debugf("[RemovePeer] Removing peer: (hash: %s) (ID: %s)", future.hash, future.peerID)
	err := ds.Delete(future.ctx, getPeerKey(future.ctx, future.hash, future.peerID))

	if err == nil {
		future.ctx.Debugf("[RemovePeer] Deleted peer from datastore.")
		for removalsAttempted := 1; removalsAttempted <= 3; removalsAttempted++ {
			future.ctx.Debugf("[RemovePeer] Attempting cache removal #%d", removalsAttempted)
			var peers []*Peer
			if peersItem, memcacheErr := memcache.Gob.Get(future.ctx, getHashKey(future.ctx, future.hash), &peers); memcacheErr == nil {
				for i, peer := range peers {
					if peer.ID == future.peerID {
						peers = append(peers[:i], peers[i+1:]...)
					}
				}

				peersItem.Object = peers
				memcacheErr := memcache.Gob.CompareAndSwap(future.ctx, peersItem)
				if memcacheErr == nil {
					future.ctx.Debugf("[RemovePeer] Deleted peer from cached peer list.")
					break
				} else if memcacheErr == memcache.ErrCASConflict {
					future.ctx.Debugf("[RemovePeer] Cached peer list conflicted during modification: %s", memcacheErr)
					continue
				} else if memcacheErr == memcache.ErrNotStored {
					future.ctx.Debugf("[RemovePeer] Cached peer list was deleted during modification: %s", memcacheErr)
					break
				} else {
					future.ctx.Errorf("[RemovePeer] Error when attempting to set cached peer list using CAS: %s", memcacheErr)
					future.resultChannel <- memcacheErr
					return
				}
			} else if memcacheErr == memcache.ErrCacheMiss {
				future.ctx.Debugf("[RemovePeer] Peer list not cached.")
				break
			} else if appengine.IsCapabilityDisabled(memcacheErr) {
				future.ctx.Warningf("[RemovePeer] Memcache capability is disabled.")
				break
			} else {
				future.ctx.Errorf("[RemovePeer] Error when attempting to get cache list for deletion: %s", memcacheErr)
				future.resultChannel <- memcacheErr
				return
			}
		}
	} else if err == dserrors.ErrNoSuchEntity {
		future.ctx.Debugf("[RemovePeer] Peer does not exist in datastore")
		return
	}

	future.ctx.Debugf("[RemovePeer] Successfully removed peer")

	if err != nil {
		future.resultChannel <- err
	}
}

type insertPeerFuture struct {
	ctx           appengine.Context
	resultChannel chan error

	hash   string
	peerID string
	ip     string
	port   uint16
}

func (f *insertPeerFuture) GetResult() (interface{}, error) {
	return nil, app.ResolveFutureError(f.resultChannel)
}

func (f *insertPeerFuture) Wait() {
	// Iterate over the result channel until it's closed.
	for {
		if _, ok := <-f.resultChannel; !ok {
			break
		}
	}
}

func InsertPeer(c appengine.Context, hash string, peerID string, ip string, port uint16) app.Future {
	future := &insertPeerFuture{
		ctx:           c,
		resultChannel: make(chan error),

		hash:   hash,
		peerID: peerID,
		ip:     ip,
		port:   port,
	}

	go actualInsertPeer(future)

	return future
}

func actualInsertPeer(future *insertPeerFuture) {
	defer close(future.resultChannel)

	future.ctx.Debugf("[InsertPeer] Inserting peer: (hash: %s) (ID: %s) (IP: %s) (port: %d)", future.hash, future.peerID, future.ip, future.port)

	changed := false
	peer := new(Peer)

	err := datastore.RunInTransaction(future.ctx, func(tc appengine.Context) error {
		peerKey := getPeerKey(tc, future.hash, future.peerID)

		if err := ds.Get(tc, peerKey, peer); err == nil {
			future.ctx.Debugf("[InsertPeer] Peer already exists, updating...")

			if peer.IP != future.ip {
				peer.IP = future.ip
				changed = true
				future.ctx.Debugf("[InsertPeer] Peer's IP has changed")
			}

			if peer.Port != int32(future.port) {
				peer.Port = int32(future.port)
				changed = true
				future.ctx.Debugf("[InsertPeer] Peer's port has changed")
			}

			currentTime := time.Now()
			if !peer.Expires.After(currentTime) || peer.Expires.Sub(currentTime) <= PeerRefreshTime {
				peer.Expires = time.Now().Add(PeerExpireTime).Add(PeerExpireGrace)
				changed = true
				future.ctx.Debugf("[InsertPeer] Peer's expire time has changed")
			}

			if changed {
				future.ctx.Debugf("[InsertPeer] Putting peer...")
				_, err := ds.Put(tc, peerKey, peer)
				if err != nil {
					future.ctx.Errorf("[InsertPeer] Error when putting updated peer: %s", err)
				}
				return err
			} else {
				future.ctx.Debugf("[InsertPeer] No need to update peer.")
				return nil
			}
		} else if err != dserrors.ErrNoSuchEntity {
			peer = nil
			future.ctx.Errorf("[InsertPeer] Error when retrieving peer from datastore for update: %s", err)
			return err
		}

		future.ctx.Debugf("[InsertPeer] Peer doesn't exist, creating")

		peer = &Peer{
			Hash:    future.hash,
			ID:      future.peerID,
			IP:      future.ip,
			Port:    int32(future.port),
			Expires: time.Now().Add(PeerExpireTime).Add(PeerExpireGrace),
		}

		_, err := ds.Put(tc, peerKey, peer)
		if err != nil {
			future.ctx.Errorf("[InsertPeer] Error when putting new peer: %s", err)
		}
		return err
	}, nil)

	if err == nil && peer != nil {
		memcacheKey := getHashKey(future.ctx, future.hash)
		for updatesAttempted := 1; updatesAttempted <= 3; updatesAttempted++ {
			future.ctx.Debugf("[InsertPeer] Attempting cache update #%d", updatesAttempted)
			var peers []*Peer
			if peersItem, memcacheErr := memcache.Gob.Get(future.ctx, memcacheKey, &peers); memcacheErr == nil {
				exists := false
				for _, thisPeer := range peers {
					if thisPeer.ID == future.peerID {
						exists = true
						break
					}
				}

				if !exists {
					peers = append(peers, peer)

					peersItem.Object = peers

					memcacheErr := memcache.Gob.CompareAndSwap(future.ctx, peersItem)
					if memcacheErr == nil {
						future.ctx.Debugf("[InsertPeer] Added peer to cached peer list.")
						break
					} else if memcacheErr == memcache.ErrCASConflict {
						future.ctx.Debugf("[InsertPeer] Cached peer list conflicted during modification: %s", memcacheErr)
						continue
					} else if memcacheErr == memcache.ErrNotStored {
						future.ctx.Debugf("[InsertPeer] Cached peer list was deleted during modification: %s", memcacheErr)
						continue
					} else if appengine.IsCapabilityDisabled(memcacheErr) {
						future.ctx.Warningf("[InsertPeer] Memcache capability disabled when attempting to add peer to cached peer list")
						continue
					} else {
						future.ctx.Errorf("[InsertPeer] Error when attempting to set cached peer list using CAS: %s", memcacheErr)
						future.resultChannel <- memcacheErr
						return
					}
				} else {
					future.ctx.Debugf("[InsertPeer] Peer already exists in cached list.")
					break
				}
			} else if memcacheErr == memcache.ErrCacheMiss {
				future.ctx.Debugf("[InsertPeer] Peer list not cached. Creating...")

				peersItem := &memcache.Item{
					Key:    memcacheKey,
					Object: []*Peer{peer},
				}
				if memcacheErr := memcache.Gob.CompareAndSwap(future.ctx, peersItem); memcacheErr == nil {
					future.ctx.Debugf("[InsertPeer] Peer list created")
					break
				} else if memcacheErr == memcache.ErrCASConflict {
					future.ctx.Debugf("[InsertPeer] Cached peer list conflicted during creation: %s", memcacheErr)
					continue
				} else if memcacheErr == memcache.ErrNotStored {
					future.ctx.Debugf("[InsertPeer] Cached peer list was deleted during creation: %s", memcacheErr)
					continue
				} else if appengine.IsCapabilityDisabled(memcacheErr) {
					future.ctx.Warningf("[InsertPeer] Memcache capability disabled when attempting to add peer to create cached peer list")
					continue
				} else {
					future.ctx.Errorf("[InsertPeer] Error when creating peer list: %s", memcacheErr)
					future.resultChannel <- memcacheErr
					return
				}
			} else if appengine.IsCapabilityDisabled(memcacheErr) {
				future.ctx.Warningf("[InsertPeer] Memcache capability is disabled.")
				break
			} else {
				future.ctx.Errorf("[InsertPeer] Error when attempting to get cache list for insertion: %s", memcacheErr)
				future.resultChannel <- memcacheErr
				return
			}
		}

		future.ctx.Debugf("[InsertPeer] Successfully inserted peer")
	} else if err != nil {
		future.resultChannel <- err
	}
}

type getPeersFuture struct {
	ctx           appengine.Context
	resultChannel chan *Peer
	errorChannel  chan error

	hash         string
	numberWanted int64
	include      string
}

func (f *getPeersFuture) GetResult() (interface{}, error) {
	if err := app.ResolveFutureError(f.errorChannel); err != nil {
		return nil, err
	}

	peers := make([]*Peer, 0)

	for peer := range f.resultChannel {
		peers = append(peers, peer)
	}

	return peers, nil
}

func (f *getPeersFuture) Wait() {
	// Iterate over the result channel until it's closed.
	for {
		if _, ok := <-f.resultChannel; !ok {
			break
		}
	}

	// Iterate over the error channel until it's closed.
	for {
		if _, ok := <-f.errorChannel; !ok {
			break
		}
	}
}

func GetPeers(c appengine.Context, hash string, numberWanted int64, include string) app.Future {
	future := &getPeersFuture{
		ctx:           c,
		resultChannel: make(chan *Peer, numberWanted),
		errorChannel:  make(chan error),

		hash:         hash,
		numberWanted: numberWanted,
		include:      include,
	}

	go actualGetPeers(future)

	return future
}

func actualGetPeers(future *getPeersFuture) {
	defer close(future.errorChannel)
	defer close(future.resultChannel)

	future.ctx.Debugf("[GetPeers] Getting peer list (%s)", future.hash)
	var res []*Peer
	if _, err := memcache.Gob.Get(future.ctx, getHashKey(future.ctx, future.hash), &res); err == nil {
		if len(res) == 0 {
			future.ctx.Debugf("[GetPeers] Cached peer list is empty")

			future.ctx.Debugf("[GetPeers] Successfully retrieved peer list from memcache (empty)")
			return
		}

		future.ctx.Debugf("[GetPeers] Valid list exists in memcache")

		if len(future.include) > 0 {
			peerToInclude := new(Peer)
			if err := ds.Get(future.ctx, getPeerKey(future.ctx, future.hash, future.include), peerToInclude); err != nil {
				if err != dserrors.ErrNoSuchEntity {
					future.ctx.Errorf("[GetPeers] Failed to include peer: %s", err)
				} else {
					future.ctx.Debugf("[GetPeers] Failed to include peer because peer doesn't exist")
				}
			} else {
				res = append(res, peerToInclude)
			}
		}

		future.ctx.Debugf("[GetPeers] Filtering peers")
		peersFiltered := make([]*Peer, 0, len(res))

		currentTime := time.Now()
		foundInclude := false

		for _, peer := range res {
			if !peer.Expires.After(currentTime) {
				continue
			} else if peer.ID == future.include {
				if foundInclude {
					continue
				} else {
					foundInclude = true
				}
			}

			peer.Key = getPeerKey(future.ctx, future.hash, peer.ID)

			// We have to copy the peer to this because the "peer" variable from the loop will always be at the same address.
			peersFiltered = append(peersFiltered, peer)
		}

		// This looks so ugly. TODO: Clean this up
		var filteredLength int64 = int64(len(peersFiltered))
		if filteredLength <= future.numberWanted {
			future.ctx.Debugf("[GetPeers] Returning all %d filtered peers: request wants more than we have", filteredLength)

			for _, peer := range peersFiltered {
				future.resultChannel <- peer
			}
			return
		}

		giveNumber := int64(math.Min(float64(filteredLength), float64(future.numberWanted)))
		given := make(map[int64]bool)
		randomGen := rand.New(rand.NewSource(currentTime.UnixNano()))
		future.ctx.Debugf("[GetPeers] Returning %d/%d random filtered peers", giveNumber, filteredLength)
		var i int64
		for i = 1; i <= giveNumber; i++ {
			giveNumber := randomGen.Int63n(filteredLength - 1)
			if isGiven, ok := given[giveNumber]; !ok || !isGiven {
				future.resultChannel <- peersFiltered[giveNumber]
				given[giveNumber] = true
			} else {
				i--
			}
		}

		future.ctx.Debugf("[GetPeers] Successfully retrieved peer list from memcache")
		return
	} else if appengine.IsCapabilityDisabled(err) {
		future.ctx.Warningf("[GetPeers] Memcache capability disabled: %s", err)
	} else if err != memcache.ErrCacheMiss {
		future.ctx.Errorf("[GetPeers] Error retrieving cached peer list: %s", err)
		future.errorChannel <- err
		return
	}

	future.ctx.Debugf("[GetPeers] No valid list exists in memcache")

	peerQuery := datastore.NewQuery("Peer").Filter("Hash =", future.hash).Filter("Expires >", time.Now())

	future.ctx.Debugf("[GetPeers] Querying for non-expired peers")

	numPeers, err := peerQuery.Count(future.ctx)
	if err != nil {
		future.ctx.Warningf("[GetPeers] Failed to get count of peers: %s", err)
		numPeers = 0
	}

	peers := make([]*Peer, 0, numPeers)

	var count int64 = 0
	for iterator := peerQuery.Run(future.ctx); ; {
		var thisPeer Peer
		key, err := iterator.Next(&thisPeer)
		if err == datastore.Done {
			break
		} else if err != nil {
			future.ctx.Errorf("[GetPeers] Error retrieving peers from query: %s", err)
			future.errorChannel <- err
			return
		}

		thisPeer.Key = key

		peers = append(peers, &thisPeer)

		if count < future.numberWanted {
			future.resultChannel <- &thisPeer
			count++
		}
	}

	future.ctx.Debugf("[GetPeers] Retrieved %d peers", len(peers))

	memcacheItem := &memcache.Item{
		Key:    getHashKey(future.ctx, future.hash),
		Object: peers,
	}

	if err := memcache.Gob.Set(future.ctx, memcacheItem); err == nil {
		future.ctx.Debugf("[GetPeers] Stored peer list in memcache")
	} else if appengine.IsCapabilityDisabled(err) {
		future.ctx.Warningf("[GetPeers] Memcache capability disabled: %s", err)
	} else {
		future.ctx.Warningf("[GetPeers] Error storing peer list in memcache: %s", err)
	}

	future.ctx.Debugf("[GetPeers] Successfully retrieved peer list from datastore")

	return
}

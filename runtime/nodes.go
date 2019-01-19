package runtime

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/bdlm/log"

	"github.com/FreifunkBremen/yanic/data"
	"github.com/FreifunkBremen/yanic/lib/jsontime"
)

// Nodes struct: cache DB of Node's structs
type Nodes struct {
	List          map[string]*Node  `json:"nodes"` // the current nodemap, indexed by node ID
	ifaceToNodeID map[string]string // mapping from MAC address to NodeID
	config        *NodesConfig
	sync.RWMutex
}

// NewNodes create Nodes structs
func NewNodes(config *NodesConfig) *Nodes {
	nodes := &Nodes{
		List:          make(map[string]*Node),
		ifaceToNodeID: make(map[string]string),
		config:        config,
	}

	if config.StatePath != "" {
		nodes.load()
	}

	return nodes
}

// Start all services to manage Nodes
func (nodes *Nodes) Start() {
	go nodes.worker()
}

func (nodes *Nodes) AddNode(node *Node) {
	nodeinfo := node.Nodeinfo
	if nodeinfo == nil || nodeinfo.NodeID == "" {
		return
	}
	nodes.Lock()
	defer nodes.Unlock()
	nodes.List[nodeinfo.NodeID] = node
	nodes.readIfaces(nodeinfo)
}

// Update a Node
func (nodes *Nodes) Update(nodeID string, res *data.ResponseData) *Node {
	now := jsontime.Now()

	nodes.Lock()
	node, _ := nodes.List[nodeID]

	if node == nil {
		node = &Node{
			Firstseen: now,
		}
		nodes.List[nodeID] = node
	}
	if res.Nodeinfo != nil {
		nodes.readIfaces(res.Nodeinfo)
	}
	nodes.Unlock()

	// Update wireless statistics
	if statistics := res.Statistics; statistics != nil {
		// Update channel utilization if previous statistics are present
		if node.Statistics != nil && node.Statistics.Wireless != nil && statistics.Wireless != nil {
			statistics.Wireless.SetUtilization(node.Statistics.Wireless)
		}
	}

	// Update fields
	node.Lastseen = now
	node.Online = true
	node.Nodeinfo = res.Nodeinfo
	node.Statistics = res.Statistics
	node.Neighbours = res.Neighbours
	node.NoRespondd = res.Statistics == nil && res.Neighbours == nil

	return node
}

// Select selects a list of nodes to be returned
func (nodes *Nodes) Select(f func(*Node) bool) []*Node {
	nodes.RLock()
	defer nodes.RUnlock()

	result := make([]*Node, 0, len(nodes.List))
	for _, node := range nodes.List {
		if f(node) {
			result = append(result, node)
		}
	}
	return result
}

func (nodes *Nodes) GetNodeIDbyAddress(addr string) string {
	return nodes.ifaceToNodeID[addr]
}

// NodeLinks returns a list of links to known neighbours
func (nodes *Nodes) NodeLinks(node *Node) (result []Link) {
	// Store link data
	neighbours := node.Neighbours
	if neighbours == nil || neighbours.NodeID == "" {
		return
	}

	for sourceMAC, batadv := range neighbours.Batadv {
		for neighbourMAC, link := range batadv.Neighbours {
			if neighbourID := nodes.ifaceToNodeID[neighbourMAC]; neighbourID != "" {
				result = append(result, Link{
					SourceID:      neighbours.NodeID,
					SourceAddress: sourceMAC,
					TargetID:      neighbourID,
					TargetAddress: neighbourMAC,
					TQ:            float32(link.Tq) / 255.0,
				})
			}
		}
	}
	for _, iface := range neighbours.Babel {
		for neighbourIP, link := range iface.Neighbours {
			if neighbourID := nodes.ifaceToNodeID[neighbourIP]; neighbourID != "" {
				result = append(result, Link{
					SourceID:      neighbours.NodeID,
					SourceAddress: iface.LinkLocalAddress,
					TargetID:      neighbourID,
					TargetAddress: neighbourIP,
					TQ:            1.0 - (float32(link.Cost) / 65535.0),
				})
			}
		}
	}
	return result
}

// Periodically saves the cached DB to json file
func (nodes *Nodes) worker() {
	c := time.Tick(nodes.config.SaveInterval.Duration)

	for range c {
		nodes.expire()
		nodes.save()
	}
}

// Expires nodes and set nodes offline
func (nodes *Nodes) expire() {
	now := jsontime.Now()

	// Nodes last seen before expireAfter will be removed
	prunePeriod := nodes.config.PruneAfter.Duration
	if prunePeriod == 0 {
		prunePeriod = time.Hour * 24 * 7 // our default
	}
	pruneAfter := now.Add(-prunePeriod)

	// Nodes last seen within OfflineAfter are changed to 'offline'
	offlineAfter := now.Add(-nodes.config.OfflineAfter.Duration)

	// Locking foo
	nodes.Lock()
	defer nodes.Unlock()

	wg := sync.WaitGroup{}

	for id, node := range nodes.List {
		if node.Lastseen.Before(pruneAfter) {
			// expire
			delete(nodes.List, id)
		} else if node.Lastseen.Before(offlineAfter) {
			// set to offline
			wg.Add(1)
			go func(node *Node) {
				defer wg.Done()
				if nodes.config.PingCount > 0 && nodes.ping(node) {
					node.Online = true
					node.NoRespondd = true

					node.Statistics = nil
					node.Neighbours = nil
				} else {
					node.Online = false
					node.NoRespondd = false
				}
			}(node)
		}
	}
	wg.Wait()
	log.WithField("nodes", "expire").Debug("end")
}

// adds the nodes interface addresses to the internal map
func (nodes *Nodes) readIfaces(nodeinfo *data.Nodeinfo) {
	nodeID := nodeinfo.NodeID
	network := nodeinfo.Network

	if nodeID == "" {
		log.Warn("nodeID missing in nodeinfo")
		return
	}

	addresses := []string{network.Mac}

	for _, iface := range network.Mesh {
		addresses = append(addresses, iface.Addresses()...)
	}

	for _, addr := range addresses {
		if addr == "" {
			continue
		}
		if oldNodeID, _ := nodes.ifaceToNodeID[addr]; oldNodeID != nodeID {
			if oldNodeID != "" {
				log.Warnf("override nodeID from %s to %s on MAC address %s", oldNodeID, nodeID, addr)
			}
			nodes.ifaceToNodeID[addr] = nodeID
		}
	}
}

func (nodes *Nodes) load() {
	path := nodes.config.StatePath

	if f, err := os.Open(path); err == nil { // transform data to legacy meshviewer
		if err = json.NewDecoder(f).Decode(nodes); err == nil {
			log.Infof("loaded %d nodes", len(nodes.List))

			nodes.Lock()
			for _, node := range nodes.List {
				if node.Nodeinfo != nil {
					nodes.readIfaces(node.Nodeinfo)
				}
			}
			nodes.Unlock()

		} else {
			log.Errorf("failed to unmarshal nodes: %s", err)
		}
	} else {
		log.Errorf("failed to load cached nodes: %s", err)
	}
}

func (nodes *Nodes) save() {
	// Locking foo
	nodes.RLock()
	defer nodes.RUnlock()

	// serialize nodes
	SaveJSON(nodes, nodes.config.StatePath)
	log.WithField("nodes", "save").Debug("end")
}

// SaveJSON to path
func SaveJSON(input interface{}, outputFile string) {
	tmpFile := outputFile + ".tmp"

	f, err := os.OpenFile(tmpFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		log.Panic(err)
	}

	err = json.NewEncoder(f).Encode(input)
	if err != nil {
		log.Panic(err)
	}

	f.Close()
	if err := os.Rename(tmpFile, outputFile); err != nil {
		log.Panic(err)
	}
}

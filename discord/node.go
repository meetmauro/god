package discord

import (
	"../common"
	"../murmur"
	"bytes"
	"fmt"
	"net"
	"net/rpc"
	"sync"
	"sync/atomic"
	"time"
)

const (
	created = iota
	started
	stopped
)

const (
	notifyInterval = time.Second
	pingInterval   = time.Second
)

type Node struct {
	ring     *common.Ring
	position []byte
	addr     string
	listener *net.TCPListener
	lock     *sync.RWMutex
	state    int32
	exports  map[string]interface{}
}

func NewNode(addr string) (result *Node) {
	return &Node{
		ring:     common.NewRing(),
		position: make([]byte, murmur.Size),
		addr:     addr,
		exports:  make(map[string]interface{}),
		lock:     new(sync.RWMutex),
		state:    created,
	}
}
func (self *Node) Export(name string, api interface{}) error {
	if self.hasState(created) {
		self.lock.Lock()
		defer self.lock.Unlock()
		self.exports[name] = api
		return nil
	}
	return fmt.Errorf("%v can only export when in state 'created'")
}
func (self *Node) SetPosition(position []byte) *Node {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.position = position
	return self
}
func (self *Node) GetNodes() (result []common.Remote) {
	return self.ring.Nodes()
}
func (self *Node) Redundancy() int {
	return self.ring.Redundancy()
}
func (self *Node) CountNodes() int {
	return self.ring.Size()
}
func (self *Node) GetPosition() (result []byte) {
	self.lock.RLock()
	defer self.lock.RUnlock()
	result = make([]byte, len(self.position))
	copy(result, self.position)
	return
}
func (self *Node) GetAddr() string {
	self.lock.RLock()
	defer self.lock.RUnlock()
	return self.addr
}
func (self *Node) String() string {
	return fmt.Sprintf("<%v@%v>", common.HexEncode(self.GetPosition()), self.GetAddr())
}
func (self *Node) Describe() string {
	self.lock.RLock()
	defer self.lock.RUnlock()
	buffer := bytes.NewBufferString(fmt.Sprintf("%v@%v\n", common.HexEncode(self.position), self.addr))
	fmt.Fprint(buffer, self.ring.Describe())
	return string(buffer.Bytes())
}

func (self *Node) hasState(s int32) bool {
	return atomic.LoadInt32(&self.state) == s
}
func (self *Node) changeState(old, neu int32) bool {
	return atomic.CompareAndSwapInt32(&self.state, old, neu)
}
func (self *Node) getListener() *net.TCPListener {
	self.lock.RLock()
	defer self.lock.RUnlock()
	return self.listener
}
func (self *Node) setListener(l *net.TCPListener) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.listener = l
}
func (self *Node) setAddr(addr string) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.addr = addr
}
func (self *Node) remote() common.Remote {
	return common.Remote{self.GetPosition(), self.GetAddr()}
}

func (self *Node) Stop() {
	if self.changeState(started, stopped) {
		self.getListener().Close()
	}
}
func (self *Node) MustStart() {
	if err := self.Start(); err != nil {
		panic(err)
	}
}
func (self *Node) Start() (err error) {
	if !self.changeState(created, started) {
		return fmt.Errorf("%v can only be started when in state 'created'", self)
	}
	if self.GetAddr() == "" {
		var foundAddr string
		if foundAddr, err = findAddress(); err != nil {
			return
		}
		self.setAddr(foundAddr)
	}
	var addr *net.TCPAddr
	if addr, err = net.ResolveTCPAddr("tcp", self.GetAddr()); err != nil {
		return
	}
	var listener *net.TCPListener
	if listener, err = net.ListenTCP("tcp", addr); err != nil {
		return
	}
	self.setListener(listener)
	server := rpc.NewServer()
	if err = server.RegisterName("Node", (*nodeServer)(self)); err != nil {
		return
	}
	for name, api := range self.exports {
		if err = server.RegisterName(name, api); err != nil {
			return
		}
	}
	self.ring.Add(self.remote())
	go server.Accept(self.getListener())
	go self.notifyPeriodically()
	go self.pingPeriodically()
	return
}
func (self *Node) notifyPeriodically() {
	for self.hasState(started) {
		self.notifySuccessor()
		time.Sleep(notifyInterval)
	}
}
func (self *Node) pingPeriodically() {
	for self.hasState(started) {
		self.pingPredecessor()
		time.Sleep(pingInterval)
	}
}
func (self *Node) Ping() {
}
func (self *Node) pingPredecessor() {
	predecessor := self.GetPredecessor()
	var x int
	if err := predecessor.Call("Node.Ping", 0, &x); err != nil {
		self.RemoveNode(predecessor)
		self.pingPredecessor()
	}
}
func (self *Node) Nodes() common.Remotes {
	return self.ring.Nodes()
}
func (self *Node) Notify(caller common.Remote) common.Remotes {
	self.ring.Add(caller)
	return self.ring.Nodes()
}
func (self *Node) notifySuccessor() {
	_, _, successor := self.ring.Remotes(self.GetPosition())
	var newNodes common.Remotes
	if err := successor.Call("Node.Notify", self.remote(), &newNodes); err != nil {
		self.RemoveNode(*successor)
	} else {
		predecessor := self.GetPredecessor()
		self.ring.SetNodes(newNodes)
		self.ring.Add(predecessor)
		if predecessor.Addr != self.GetAddr() {
			self.ring.Clean(predecessor.Pos, self.GetPosition())
		}
	}
}
func (self *Node) MustJoin(addr string) {
	if err := self.Join(addr); err != nil {
		panic(err)
	}
}
func (self *Node) Join(addr string) (err error) {
	if bytes.Compare(self.GetPosition(), make([]byte, murmur.Size)) == 0 {
		var newNodes common.Remotes
		if err = common.Switch.Call(addr, "Node.Ring", 0, &newNodes); err != nil {
			return
		}
		self.SetPosition(common.NewRingNodes(newNodes).GetSlot())
	}
	var newNodes common.Remotes
	if err = common.Switch.Call(addr, "Node.Notify", self.remote(), &newNodes); err != nil {
		return
	}
	self.ring.SetNodes(newNodes)
	return
}
func (self *Node) RemoveNode(remote common.Remote) {
	if remote.Addr == self.GetAddr() {
		panic(fmt.Errorf("%v is trying to remove itself from the routing!", self))
	}
	self.ring.Remove(remote)
}
func (self *Node) GetPredecessor() common.Remote {
	pred, _, _ := self.ring.Remotes(self.GetPosition())
	return *pred
}
func (self *Node) GetSuccessor(key []byte) common.Remote {
	predecessor, match, successor := self.ring.Remotes(key)
	if match != nil {
		predecessor = match
	}
	if predecessor.Addr != self.GetAddr() {
		if err := predecessor.Call("Node.GetSuccessor", key, successor); err != nil {
			self.RemoveNode(*successor)
			return self.GetSuccessor(key)
		}
	}
	return *successor
}

package goflow

import (
	"errors"
	"fmt"
	"reflect"
)

// port stores full port information within the network.
type port struct {
	// Process name in the network
	proc string
	// Port name of the process
	port string
	// Actual channel attached
	channel reflect.Value
	// Runtime info
	info PortInfo
}

// address is a full port accessor including the index part
type address struct {
	proc string
	port string
	key  string
	// index int
}

// connection stores information about a connection within the net.
type connection struct {
	src     address
	tgt     address
	channel reflect.Value
	buffer  int
}

// Connect connects a sender to a receiver and creates a channel between them using BufferSize configuratio nof the graph.
// Normally such a connection is unbuffered but you can change by setting flow.DefaultBufferSize > 0 or
// by using ConnectBuf() function instead.
// It returns true on success or panics and returns false if error occurs.
func (n *Graph) Connect(senderName, senderPort, receiverName, receiverPort string) error {
	return n.ConnectBuf(senderName, senderPort, receiverName, receiverPort, n.conf.BufferSize)
}

// ConnectBuf connects a sender to a receiver using a channel with a buffer of a given size.
// It returns true on success or panics and returns false if error occurs.
func (n *Graph) ConnectBuf(senderName, senderPort, receiverName, receiverPort string, bufferSize int) error {
	sendAddr := parseAddress(senderName, senderPort)
	sendPort, err := n.getProcPort(senderName, senderPort, reflect.SendDir)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	recvAddr := parseAddress(receiverName, receiverPort)
	recvPort, err := n.getProcPort(receiverName, receiverPort, reflect.RecvDir)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	isNewChan := false // tells if a new channel will need to be created for this connection
	// Try to find an existing outbound channel from the same sender,
	// so it can be used as fan-out FIFO
	ch := n.findExistingChan(sendAddr, reflect.SendDir)
	if !ch.IsValid() || ch.IsNil() {
		// Then try to find an existing inbound channel to the same receiver,
		// so it can be used as a fan-in FIFO
		ch = n.findExistingChan(recvAddr, reflect.RecvDir)
		if ch.IsValid() && !ch.IsNil() {
			// Increase the number of listeners on this already used channel
			n.incChanListenersCount(ch)
		} else {
			isNewChan = true
		}
	}
	ch, err = attachPort(sendPort, reflect.SendDir, ch, bufferSize)
	if err != nil {
		return fmt.Errorf("connect '%s.%s': %w", senderName, senderPort, err)
	}

	if _, err = attachPort(recvPort, reflect.RecvDir, ch, bufferSize); err != nil {
		return fmt.Errorf("connect '%s.%s': %w", receiverName, receiverPort, err)
	}
	if isNewChan {
		// Register the first listener on a newly created channel
		n.incChanListenersCount(ch)
	}

	// Add connection info
	n.connections = append(n.connections, connection{
		src:     sendAddr,
		tgt:     recvAddr,
		channel: ch,
		buffer:  bufferSize})

	return nil
}

// getProcPort finds an assignable port field in one of the subprocesses
func (n *Graph) getProcPort(procName, portName string, dir reflect.ChanDir) (reflect.Value, error) {
	nilValue := reflect.ValueOf(nil)
	// Check if process exists
	proc, ok := n.procs[procName]
	if !ok {
		return nilValue, fmt.Errorf("process '%s' not found", procName)
	}

	// Check if process is settable
	val := reflect.ValueOf(proc)
	if val.Kind() == reflect.Ptr && val.IsValid() {
		val = val.Elem()
	}
	if !val.CanSet() {
		return nilValue, fmt.Errorf("process '%s' is not settable", procName)
	}

	// Get the port value
	var portVal reflect.Value
	var err error
	// Check if sender is a net
	net, ok := val.Interface().(Graph)
	if ok {
		// Sender is a net
		if dir == reflect.SendDir {
			portVal, err = net.getOutPort(portName)
		} else {
			portVal, err = net.getInPort(portName)
		}

	} else {
		// Sender is a proc
		portVal = val.FieldByName(portName)
		if !portVal.IsValid() {
			err = errors.New("")
		}
	}
	if err != nil {
		return nilValue, fmt.Errorf("process '%s' does not have port '%s'", procName, portName)
	}

	return portVal, nil
}

func attachPort(port reflect.Value, dir reflect.ChanDir, ch reflect.Value, bufSize int) (reflect.Value, error) {
	if err := validateChanDir(port.Type(), dir); err != nil {
		return ch, err
	}
	if err := validateCanSet(port); err != nil {
		return ch, err
	}
	ch = selectOrMakeChan(ch, port, bufSize)
	port.Set(ch)
	return ch, nil
}

func validateChanDir(portType reflect.Type, dir reflect.ChanDir) error {
	if portType.Kind() != reflect.Chan {
		return fmt.Errorf("not a channel")
	}

	if portType.ChanDir()&dir == 0 {
		return fmt.Errorf("channel does not support direction %s", dir.String())
	}
	return nil
}

func validateCanSet(portVal reflect.Value) error {
	if !portVal.CanSet() {
		return fmt.Errorf("port is not assignable")
	}
	return nil
}

func selectOrMakeChan(new, existing reflect.Value, bufSize int) reflect.Value {
	if !new.IsValid() || new.IsNil() {
		if existing.IsValid() && !existing.IsNil() {
			return existing
		}
		chanType := reflect.ChanOf(reflect.BothDir, existing.Type().Elem())
		new = reflect.MakeChan(chanType, bufSize)
	}
	return new
}

// parseAddress unfolds a string port name into parts, including array index or hashmap key
func parseAddress(proc, port string) address {
	n := address{
		proc: proc,
		port: port,
		// index: -1,
	}
	keyPos := 0
	key := ""
	for i, r := range port {
		if r == '[' {
			keyPos = i + 1
			n.port = port[0:i]
		}
		if r == ']' {
			key = port[keyPos:i]
		}
	}
	if key == "" {
		return n
	}
	// if i, err := strconv.Atoi(key); err == nil {
	// 	n.index = i
	// } else {
	// 	n.key = key
	// }
	n.key = key
	return n
}

// findExistingChan returns a channel attached to receiver if it already exists among connections
func (n *Graph) findExistingChan(addr address, dir reflect.ChanDir) reflect.Value {
	var channel reflect.Value
	// Find existing channel attached to the receiver
	for _, conn := range n.connections {
		var a address
		if dir == reflect.SendDir {
			a = conn.src
		} else {
			a = conn.tgt
		}
		if a == addr {
			channel = conn.channel
			break
		}
	}
	return channel
}

// incChanListenersCount increments SendChanRefCount.
// The count is needed when multiple senders are connected
// to the same receiver. When the network is terminated and
// senders need to close their output port, this counter
// can help to avoid closing the same channel multiple times.
func (n *Graph) incChanListenersCount(c reflect.Value) {
	n.chanListenersCountLock.Lock()
	defer n.chanListenersCountLock.Unlock()

	ptr := c.Pointer()
	cnt := n.chanListenersCount[ptr]
	cnt++
	n.chanListenersCount[ptr] = cnt
}

// decChanListenersCount decrements SendChanRefCount
// It returns true if the RefCount has reached 0
func (n *Graph) decChanListenersCount(c reflect.Value) bool {
	n.chanListenersCountLock.Lock()
	defer n.chanListenersCountLock.Unlock()

	ptr := c.Pointer()
	cnt := n.chanListenersCount[ptr]
	if cnt == 0 {
		return true //yes you may try to close a nonexistant channel, see what happens...
	}
	cnt--
	n.chanListenersCount[ptr] = cnt
	return cnt == 0
}

// // Disconnect removes a connection between sender's outport and receiver's inport.
// func (n *Graph) Disconnect(senderName, senderPort, receiverName, receiverPort string) bool {
// 	var sender, receiver interface{}
// 	var ok bool
// 	sender, ok = n.procs[senderName]
// 	if !ok {
// 		return false
// 	}
// 	receiver, ok = n.procs[receiverName]
// 	if !ok {
// 		return false
// 	}
// 	res := unsetProcPort(sender, senderPort, true)
// 	res = res && unsetProcPort(receiver, receiverPort, false)
// 	return res
// }

// // Unsets an port of a given process
// func unsetProcPort(proc interface{}, portName string, isOut bool) bool {
// 	v := reflect.ValueOf(proc)
// 	var ch reflect.Value
// 	if v.Elem().FieldByName("Graph").IsValid() {
// 		if subnet, ok := v.Elem().FieldByName("Graph").Addr().Interface().(*Graph); ok {
// 			if isOut {
// 				ch = subnet.getOutPort(portName)
// 			} else {
// 				ch = subnet.getInPort(portName)
// 			}
// 		} else {
// 			return false
// 		}
// 	} else {
// 		ch = v.Elem().FieldByName(portName)
// 	}
// 	if !ch.IsValid() {
// 		return false
// 	}
// 	ch.Set(reflect.Zero(ch.Type()))
// 	return true
// }

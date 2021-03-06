// Copyright 2016 The Netstack Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package queue provides the implementation of transmit and receive queues
// based on shared memory ring buffers.
package queue

import (
	"encoding/binary"
	"sync/atomic"

	"github.com/google/netstack/tcpip/link/sharedmem/pipe"
	"log"
)

const (
	// Offsets within a posted buffer.
	postedOffset           = 0
	postedSize             = 8
	postedRemainingInGroup = 12
	postedUserData         = 16
	postedID               = 24

	sizeOfPostedBuffer = 32

	// Offsets within a received packet header.
	consumedPacketSize     = 0
	consumedPacketReserved = 4

	sizeOfConsumedPacketHeader = 8

	// Offsets within a consumed buffer.
	consumedOffset   = 0
	consumedSize     = 8
	consumedUserData = 12
	consumedID       = 20

	sizeOfConsumedBuffer = 28

	// The following are the allowed states of the shared data area.
	eventFDUninitialized = 0
	eventFDDisabled      = 1
	eventFDEnabled       = 2
)

// RxBuffer is the descriptor of a receive buffer.
type RxBuffer struct {
	Offset uint64
	Size   uint32
	ID     uint64
}

// Rx is a receive queue. It is implemented with one tx and one rx pipe: the tx
// pipe is used to "post" buffers, while the rx pipe is used to receive packets
// whose contents have been written to previously posted buffers.
//
// This struct is thread-compatible.
type Rx struct {
	tx                 pipe.Tx
	rx                 pipe.Rx
	sharedEventFDState *uint32
}

// Init initializes the receive queue with the given pipes, and shared state
// pointer -- the latter is used to enable/disable eventfd notifications.
func (r *Rx) Init(tx, rx []byte, sharedEventFDState *uint32) {
	r.sharedEventFDState = sharedEventFDState
	r.tx.Init(tx)
	r.rx.Init(rx)
}

// EnableNotification updates the shared state such that the peer will notify
// the eventfd when there are packets to be dequeued.
func (r *Rx) EnableNotification() {
	atomic.StoreUint32(r.sharedEventFDState, eventFDEnabled)
}

// DisableNotification updates the shared state such that the peer will not
// notify the eventfd.
func (r *Rx) DisableNotification() {
	atomic.StoreUint32(r.sharedEventFDState, eventFDDisabled)
}

// PostBuffers makes the given buffers available for receiving data from the
// peer. Once they are posted, the peer is free to write to them and will
// eventually post them back for consumption.
func (r *Rx) PostBuffers(buffers []RxBuffer) bool {
	for i := range buffers {
		b := r.tx.Push(sizeOfPostedBuffer)
		if b == nil {
			r.tx.Abort()
			return false
		}

		pb := &buffers[i]
		binary.LittleEndian.PutUint64(b[postedOffset:], pb.Offset)
		binary.LittleEndian.PutUint32(b[postedSize:], pb.Size)
		binary.LittleEndian.PutUint32(b[postedRemainingInGroup:], 0)
		binary.LittleEndian.PutUint64(b[postedUserData:], 0)
		binary.LittleEndian.PutUint64(b[postedID:], pb.ID)
	}

	r.tx.Flush()

	return true
}

// Dequeue receives buffers that have been previously posted by PostBuffers()
// and that have been filled by the peer and posted back.
//
// This is similar to append() in that new buffers are appended to "bufs", with
// reallocation only if "bufs" doesn't have enough capacity.
func (r *Rx) Dequeue(bufs []RxBuffer) ([]RxBuffer, uint32) {
	for {
		outBufs := bufs

		// Pull the next descriptor from the rx pipe.
		b := r.rx.Pull()
		if b == nil {
			return bufs, 0
		}

		if len(b) < sizeOfConsumedPacketHeader {
			log.Printf("Ignoring packet header: size (%v) is less than header size (%v)", len(b), sizeOfConsumedPacketHeader)
			r.rx.Flush()
			continue
		}

		totalDataSize := binary.LittleEndian.Uint32(b[consumedPacketSize:])

		// Calculate the number of buffer descriptors and copy them
		// over to the output.
		count := (len(b) - sizeOfConsumedPacketHeader) / sizeOfConsumedBuffer
		offset := sizeOfConsumedPacketHeader
		buffersSize := uint32(0)
		for i := count; i > 0; i-- {
			s := binary.LittleEndian.Uint32(b[offset+consumedSize:])
			buffersSize += s
			if buffersSize < s {
				// The buffer size overflows an unsigned 32-bit
				// integer, so break out and force it to be
				// ignored.
				totalDataSize = 1
				buffersSize = 0
				break
			}

			outBufs = append(outBufs, RxBuffer{
				Offset: binary.LittleEndian.Uint64(b[offset+consumedOffset:]),
				Size:   s,
				ID:     binary.LittleEndian.Uint64(b[offset+consumedID:]),
			})

			offset += sizeOfConsumedBuffer
		}

		r.rx.Flush()

		if buffersSize < totalDataSize {
			// The descriptor is corrupted, ignore it.
			log.Printf("Ignoring packet: actual data size (%v) less than expected size (%v)", buffersSize, totalDataSize)
			continue
		}

		return outBufs, totalDataSize
	}
}

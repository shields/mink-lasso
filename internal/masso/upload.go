// Copyright © 2026 Michael Shields
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package masso

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"time"
)

// Upload sends a size-byte file named name, read from r, to the
// controller's USB drive (docs/protocol.md §5). dir is the directory on the
// drive: "" for the root, otherwise a backslash-separated relative path
// such as `JOBS\SUB` that satisfies ValidateUploadDir (ErrBadUploadDir if
// not). name must satisfy ValidateFileName.
//
// Upload resends the start request every Options.StartRetransmit until the
// controller acknowledges it, giving up with ErrNoResponse
// Options.StartTimeout after the first send; a resend is always scheduled
// Options.StartRetransmit after the resend it follows; even after a gap
// long enough to fall behind that cadence, Upload sends one catch-up resend
// rather than a burst. It then sends the file in chunks of MaxChunkData
// bytes, up to two in flight, retransmitting any that have been
// unacknowledged for strictly longer than an adaptive timeout, and gives up
// with ErrNoResponse once Options.StallTimeout has passed since the last
// chunk ACK of any kind, counting from the start ACK's arrival; nothing
// about sending a chunk, only acknowledging one, resets that window, so
// time spent reading r counts toward it. Only replies from the controller
// Upload started with count, even if a later Connect records another.
//
// progress, if non-nil, is called once with (0, size) right after the start
// ACK and again each time the controller's count of accepted chunks
// advances. Only one upload runs at a time per Client; a concurrent call
// returns ErrBusy. A zero-byte file sends only the start packet — the real
// controller's behavior for a zero-byte upload is unconfirmed. If ctx is
// already done Upload sends nothing; ctx cancellation while waiting for the
// start ACK returns ctx.Err(). ctx is never observed once the start is
// acknowledged, since aborting then risks leaving a partial file on the
// controller's USB drive.
//
// If the start request drew any reply from the controller and the transfer
// did not go on to end cleanly — a start ACK carrying an error result, a
// chunk-ACK error result, a read or send error, or either give-up above —
// Upload sends the upload-abort notification (docs/protocol.md §5.5) three
// times, Options.AbortInterval apart, before returning the error. It never
// sends it when the start drew no reply at all — the StartTimeout give-up,
// ctx cancellation before any start ACK, or a start send error — or for a
// completed transfer.
func (c *Client) Upload(
	ctx context.Context, dir, name string, r io.ReaderAt, size int64, progress func(sent, total int64),
) error {
	if size < 0 || size > math.MaxUint32 {
		return fmt.Errorf("%w: %d bytes", ErrFileTooLarge, size)
	}
	remote := c.Remote()
	if remote == nil {
		return ErrNotConnected
	}

	if !c.uploadMu.TryLock() {
		return ErrBusy
	}
	defer c.uploadMu.Unlock()

	sizeU32 := uint32(size & math.MaxUint32) // size is already checked to fit uint32 above
	startPkt, err := UploadStart(sizeU32, dir, name)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}

	answered, err := c.startUpload(ctx, remote, startPkt)
	if err != nil {
		if answered {
			c.notifyAbort(remote)
		}
		return err
	}
	startAckAt := c.clock.Now()

	if progress != nil {
		progress(0, size)
	}
	if size == 0 {
		return nil
	}

	if err := c.sendChunks(remote, r, size, progress, startAckAt); err != nil {
		c.notifyAbort(remote)
		return err
	}
	return nil
}

// startUpload sends the upload-start request pkt, resending it every
// c.startRetransmit until a StartAck arrives, ctx is done, or
// c.startTimeout passes since the first send. It reports whether any
// start-ACK reply arrived at all, success or failure — Upload uses that to
// decide whether a refused start still gets the abort notification of
// docs/protocol.md §5.5.
func (c *Client) startUpload(ctx context.Context, remote *net.UDPAddr, pkt []byte) (answered bool, err error) {
	ch, cancel := c.expect(TypeUploadStart, remote, anyReply)
	defer cancel()

	first := c.clock.Now()
	if err := c.send(pkt, remote); err != nil {
		return false, err
	}
	giveUp := first.Add(c.startTimeout)
	resends := 0
	nextResend := first.Add(c.startRetransmit)

	for {
		now := c.clock.Now()
		if !now.Before(giveUp) {
			return false, ErrNoResponse
		}
		if !now.Before(nextResend) {
			// A reply that arrived while the resend fell due answers the
			// attempts already sent, so it must be judged before the
			// resend counts.
			select {
			case in := <-ch:
				return true, startAckErr(in, resends)
			default:
			}
			if err := c.send(pkt, remote); err != nil {
				return false, err
			}
			resends++
			// Scheduled after this resend's own send time, so a client
			// that falls behind by more than one interval sends a single
			// catch-up resend, never a burst (docs/protocol.md §5.3).
			nextResend = now.Add(c.startRetransmit)
			continue
		}

		timer := c.clock.NewTimer(earlier(nextResend, giveUp).Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C():
		case in := <-ch:
			timer.Stop()
			return true, startAckErr(in, resends)
		}
	}
}

// startAckErr maps a start ACK to startUpload's result. A
// StartAlreadyStarted result counts as started only once the request has
// been resent at least once (docs/protocol.md §5.1); any other result but
// StartOK is returned as StartAck.Err.
func startAckErr(in incoming, resends int) error {
	// The reader only routes StartAck values to a TypeUploadStart waiter;
	// the assertion cannot fail.
	ack := mustType[StartAck](in.reply)
	if ack.Result == StartAlreadyStarted && resends > 0 {
		return nil
	}
	return ack.Err()
}

type inflightChunk struct {
	pkt           []byte
	sentAt        time.Time
	retransmitted bool
}

// sendChunks sends the chunks of a size-byte file read from r, with the
// sliding window, adaptive retransmission, and stall give-up of
// docs/protocol.md §5.3. startAckAt is when the start ACK arrived, the
// stall window's starting point.
func (c *Client) sendChunks(
	remote *net.UDPAddr, r io.ReaderAt, size int64, progress func(sent, total int64), startAckAt time.Time,
) error {
	ch, cancel := c.expect(TypeUploadChunk, remote, anyReply)
	defer cancel()

	total := (size + MaxChunkData - 1) / MaxChunkData
	var (
		accepted, next int64
		window         int64 = 1
		streak         int
		srtt           = initialSRTT
		// lastActivity is the last chunk ACK of any kind, starting from the
		// start ACK's arrival; sending a chunk does not move it, so time
		// spent reading the file counts toward the stall give-up.
		lastActivity = startAckAt
		flight       []inflightChunk
		buf          = make([]byte, MaxChunkData)
	)

	// ack folds one chunk ACK into the window and reports whether the
	// whole file has now been accepted.
	ack := func(in incoming) (bool, error) {
		now := c.clock.Now()
		lastActivity = now
		// The reader only routes ChunkAck values to a TypeUploadChunk
		// waiter; the assertion cannot fail.
		a := mustType[ChunkAck](in.reply)
		if err := a.Err(); err != nil {
			return false, err
		}
		newAccepted := min(int64(a.Accepted), next)
		if newAccepted <= accepted {
			c.logger.Debug("masso: upload: chunk ACK did not advance", "accepted", a.Accepted)
			return false, nil
		}
		for ; accepted < newAccepted; accepted++ {
			f := flight[0]
			flight = flight[1:]
			if f.retransmitted {
				continue
			}
			srtt = smoothRTT(srtt, now.Sub(f.sentAt))
			streak++
			if streak >= cleanStreakToOpen {
				window = maxWindow
			}
		}
		if progress != nil {
			progress(min(accepted*MaxChunkData, size), size)
		}
		return accepted == total, nil
	}

	for {
		for next < total && next-accepted < window {
			pkt, err := readChunk(r, next, size, buf)
			if err != nil {
				return err
			}
			if err := c.send(pkt, remote); err != nil {
				return err
			}
			flight = append(flight, inflightChunk{pkt: pkt, sentAt: c.clock.Now()})
			next++
		}

		// An ACK that arrived while this goroutine was reading or sending
		// must be counted before either deadline below is judged.
		select {
		case in := <-ch:
			if done, err := ack(in); done || err != nil {
				return err
			}
			continue
		default:
		}

		now := c.clock.Now()
		stallAt := lastActivity.Add(c.stallTimeout)
		if !now.Before(stallAt) {
			return ErrNoResponse
		}
		resendAt := flight[0].sentAt.Add(retransmitTimeout(srtt))
		if now.After(resendAt) {
			for i := range flight {
				if err := c.send(flight[i].pkt, remote); err != nil {
					return err
				}
				flight[i].sentAt = now
				flight[i].retransmitted = true
			}
			window = 1
			streak = 0
			continue
		}

		// A retransmit needs the oldest chunk outstanding strictly longer
		// than the timeout, so wake just past resendAt, not at it.
		timer := c.clock.NewTimer(earlier(resendAt.Add(time.Nanosecond), stallAt).Sub(now))
		select {
		case <-timer.C():
		case in := <-ch:
			timer.Stop()
			if done, err := ack(in); done || err != nil {
				return err
			}
		}
	}
}

// readChunk reads chunk index of a size-byte file from r into buf and
// returns its encoded request. A short read, or an error other than io.EOF
// accompanying a full one, fails with ErrRead.
func readChunk(r io.ReaderAt, index, size int64, buf []byte) ([]byte, error) {
	off := index * MaxChunkData
	want := int(min(size-off, MaxChunkData))
	n, err := r.ReadAt(buf[:want], off)
	switch {
	case n < want && err == nil:
		return nil, fmt.Errorf("%w: short read at offset %d: %w", ErrRead, off, io.ErrUnexpectedEOF)
	case n < want || (err != nil && !errors.Is(err, io.EOF)):
		return nil, fmt.Errorf("%w: %w", ErrRead, err)
	}
	// want never exceeds MaxChunkData, so UploadChunk cannot reject it; the
	// index fits uint32 because size does.
	return must(UploadChunk(uint32(index&math.MaxUint32), buf[:want])), nil
}

func (c *Client) notifyAbort(remote net.Addr) {
	pkt := UploadAbort()
	for i := range abortNotifications {
		if i > 0 {
			<-c.clock.After(c.abortInterval)
		}
		if _, err := c.conn.WriteTo(pkt, remote); err != nil {
			c.logger.Debug("masso: upload-abort send failed", "error", err)
		}
	}
}

func retransmitTimeout(srtt time.Duration) time.Duration {
	return min(max(2*srtt, minRTO), maxRTO)
}

func smoothRTT(srtt, sample time.Duration) time.Duration {
	return (3*srtt + sample) / 4
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

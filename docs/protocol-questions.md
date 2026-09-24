# Open protocol questions

Questions that [protocol.md](protocol.md) leaves open, raised while implementing
a client against it. Each is phrased in terms of the protocol alone. An analyst
answers a question by updating `protocol.md`; once it is answered there, the
question is removed from this file. A question that only a real controller can
settle stays here, and `protocol.md` says the point is unverified.

## Upload start and the post-transfer signal

1. **`0x0C` after a refused start.** §5.5 sends the `0x0C` signal when "the
   start request got an ACK (the upload actually began)" but the transfer did
   not end cleanly. Is it also sent when the start ACK itself carries an error
   result (`0xE9`, or any other result that is not a success)?
2. **`0x0C` after a local failure.** Once the start is acknowledged, is `0x0C`
   sent when the transfer fails for a reason on the client's side — the file
   cannot be read partway through, or a send on the socket fails — or only for
   the causes §5.5 lists?
3. **Missed start resends.** v2.15 resends the start packet "roughly once per
   second" (§5.3). If the client falls behind that schedule (say the process was
   suspended across several one-second slots), does it send one resend per
   missed slot, or one resend and then continue a second after it?
4. **`0xF7` on the controller.** Does real firmware answer a resent start with
   `0xF7`, and after doing so does the controller expect the transfer to begin
   at chunk 0?

## Chunk phase

5. **Retransmit boundary.** §5.3 resends every chunk in flight "whenever the
   oldest unacknowledged chunk has been outstanding longer than" the retransmit
   timeout. Is the comparison strict (longer than), or at least equal?
6. **Round-trip samples from a cumulative ACK.** When one chunk ACK's accepted
   count covers several chunks at once, none of them retransmitted, is the
   smoothed round-trip estimate updated once per chunk (each with its own
   sample) or once per ACK — and in the latter case, with which chunk's sample?
7. **Non-advancing ACKs and the window.** Does a chunk ACK that does not advance
   the accepted count (a duplicate, or one with a lower count) affect the run of
   consecutive clean acknowledgments that opens the window from 1 to 2?
8. **Non-advancing ACKs and the give-up.** §5.3 gives up "after 15 s with no ACK
   activity of any kind". Does a non-advancing chunk ACK restart that 15 s, so
   that a controller that keeps answering without accepting anything holds the
   transfer open indefinitely? Does time the client spends reading the file
   count toward the 15 s?
9. **Error results and the accepted count.** When a chunk ACK carries an error
   result (`0x01`, `0x02`, or another), is its accepted count used — for
   example, to report progress — before the transfer is abandoned?
10. **ACKs from an earlier transfer.** A chunk ACK delayed from a previous
    transfer can arrive during the next one, and clamping its accepted count to
    the chunks sent (§5.3) would count it against the new transfer. Does Masso
    Link do anything to tell such an ACK apart, such as discarding chunk ACKs
    received before the start ACK?

## Controller behavior

These probably need a real controller; say so in `protocol.md` if the binaries
cannot answer them.

11. **Refused starts.** Does the controller begin an upload, and store chunks
    sent afterward, when it answers a start request with an error result?
12. **After `0x0C`.** What does the controller do on receiving `0x0C`: keep the
    partial file, delete it, or neither? What accepted count does it report for
    chunks sent after it?
13. **Missing directories.** Does the controller create a directory named in the
    start packet's path field (§5.1) that does not yet exist? If not, which
    result does the start ACK, or the first chunk ACK, carry?
14. **Directory names.** Which bytes may a directory name in the path field
    contain, and is there a limit on one component's length beyond the path
    field's 255 bytes?

## Replies and connection

15. **Filtering identity and config replies.** After connecting, does Masso
    Link's source-IP filter (§1) also discard identity (`0x02`) and config
    (`0x03`) replies that come from another address, or only the other reply
    types?
16. **Config reply and 32-bit serials.** §3.1 makes the serial a 32-bit field,
    but the config reply (§3.2) echoes it in bytes 5–6. Which 16 bits are those,
    and does Masso Link compare them with anything?
17. **Identity bytes 9–12.** What does the second 32-bit field of the identity
    reply mean, and is byte 12 always `0x40`?
18. **33-character file names in status.** When the status packet's current file
    name (§4) is exactly 33 characters, is byte 50 a NUL, or does the name run
    into the reserved area?
19. **Liveness.** §8 resets the liveness countdown on any datagram that passes
    the CRC check. Does that include a datagram from another address, which §1
    says is discarded before the CRC check, and one whose length does not match
    its type?

## Folder uploads

20. **Base path.** What form does the base-path setting of §5.1 take in the path
    field — with or without a leading or trailing backslash — and how is it
    joined with a folder drop's relative path?
21. **Walk limits.** Are the 16-level and 500-entry caps of §5.1 applied per
    dropped folder or across the whole queue, and are entries beyond them
    skipped silently or reported to the operator?

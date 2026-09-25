# Open protocol questions

Questions that [protocol.md](protocol.md) leaves open, raised while implementing
a client against it. Each is phrased in terms of the protocol alone. An analyst
answers a question by updating `protocol.md`; once it is answered there, the
question is removed from this file. A question that only a real controller can
settle stays here, and `protocol.md` says the point is unverified. Questions
keep their numbers when others are removed. `make probe` (README's "Controller
probes") records what a real controller does for the ones only it can settle.

## Upload start

2. **`0xF7` on the controller.** Does real firmware answer a resent start with
   `0xF7`, and after doing so does the controller expect the transfer to begin
   at chunk 0? Masso Link always begins again at chunk 0 (§5.1); what the
   controller expects is unverified.

## Controller behavior

These need a real controller; `protocol.md` records each as unverified.

3. **Refused starts.** Does the controller begin an upload, and store chunks
   sent afterward, when it answers a start request with an error result? Masso
   Link never sends a chunk after one (§5.1).
4. **After `0x0C`.** What does the controller do on receiving `0x0C`: keep the
   partial file, delete it, or neither? What accepted count does it report for
   chunks sent after it?
5. **Missing directories.** Does the controller create a directory named in the
   start packet's path field (§5.1) that does not yet exist? If not, which
   result does the start ACK, or the first chunk ACK, carry?
6. **Directory names.** Which bytes may a directory name in the path field
   contain, and is there a limit on one component's length beyond the path
   field's 255 bytes? Masso Link checks neither (§5.1).

## Replies

7. **Config reply and 32-bit serials.** §3.1 makes the serial a 32-bit field,
   but the config reply (§3.2) echoes it in bytes 5–6. Which 16 bits are those,
   and does Masso Link compare them with anything?
8. **Identity bytes 9–12.** What does the second 32-bit field of the identity
   reply mean, and is byte 12 always `0x40`?
9. **33-character file names in status.** When the status packet's current file
   name (§4) is exactly 33 characters, is byte 50 a NUL, or does the name run
   into the reserved area?

## Folder uploads

10. **Base path.** What form does the base-path setting of §5.1 take in the path
    field — with or without a leading or trailing backslash — and how is it
    joined with a folder drop's relative path? §5.1 records that the setting's
    text becomes the path field, but not its exact form or the join.

## File names

12. **File-name length.** Does the controller accept a file name longer than 15
    characters (§5), and store the file under its full name? Masso Link checks
    nothing narrower than 255 characters (§5.1).

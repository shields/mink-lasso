# Open protocol questions

Questions that [protocol.md](protocol.md) leaves open, raised while implementing
a client against it. Each is phrased in terms of the protocol alone. An analyst
answers a question by updating `protocol.md`; once it is answered there, the
question is removed from this file. A question that only a real controller can
settle stays here, and `protocol.md` says the point is unverified. Questions
keep their numbers when others are removed. `make probe` (README's "Controller
probes") records what a real controller does for the ones only it can settle.

## Controller behavior

These need a real controller; `protocol.md` records each as unverified.

4. **After `0x0C`.** `0x0C` ends a transfer's data and frees its name, though
   the controller's screen keeps showing it (§5.5). Does it ever leave a partial
   or 0-byte file behind?

## Open transfers

13. **Freeing a displaced transfer.** A transfer displaced by a start for
    another file without `0x0C` first stays open, and its name draws `0xF7`,
    until the controller restarts (§5.5). Is there any packet that frees it?

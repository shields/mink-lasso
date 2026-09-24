# Masso Link UDP Protocol

Full wire-format documentation for the protocol spoken between the **Masso
Link** desktop application and a **Masso G3 Touch** CNC controller fitted with
the Wi-Fi / Ethernet module.

Reverse-engineered from **Masso Link v2.12**, **v2.14**, and **v2.15** (released
2026-09-03) — Hind Technology Australia's 64-bit Free Pascal / Lazarus
application using the Synapse `TUDPBlockSocket`. Every statement below is backed
by one or more of:

- **Static analysis (v2.12)** — Ghidra decompilation of the sender thread
  (`Send_NC_File_Thread_Class.Execute`), the receiver thread
  (`UDP_Server_Thread_Class.Execute`), the send routine, the CRC routine, and
  the status/timer UI code.
- **Live capture (v2.12)** — packets recorded with tshark against a real
  controller (a 5-Axis machine running firmware **v5.13**) plus an independent
  Python re-implementation that connects, reads status/tools, and uploads a file
  — verified byte-for-byte against the app.
- **Static analysis (v2.14/v2.15)** — Capstone disassembly and angr
  decompilation of the same two threads, the send/CRC routines, and the
  connect-dialog and status-timer resource forms, run against the Linux ELF,
  Windows PE, and macOS Apple Silicon (arm64) builds of v2.14 and v2.15.0.
  Statements specific to v2.14/v2.15 come from the client binaries alone, with
  no live capture, and are unverified against real firmware.

> This is unofficial documentation. It is not affiliated with or endorsed by
> Masso. Uploading a file only writes it to the controller's USB drive; it does
> **not** run anything. Only one client may talk to a controller at a time.

---

## 1. Transport and connection model

| Property                  | Value                                               |
| ------------------------- | --------------------------------------------------- |
| Protocol                  | UDP                                                 |
| Controller listening port | **65535** (all client → controller packets go here) |
| Client listening port     | **11000** by default (see below)                    |
| Byte order                | little-endian for all multi-byte integers           |

**How the controller decides where to reply.** The client does _not_ rely on the
controller replying to the request's source port. Instead, the **discovery
packet carries the port the client wants replies on** (bytes 5–6,
little-endian). The controller stores `<client-IP, that-port>` and sends _every_
subsequent reply and status broadcast to it.

- Masso Link binds the first free UDP port in the range **11000–11051** for
  receiving, and advertises that port in the discovery packet. It sends its
  requests from a _separate_ ephemeral socket. In a capture you therefore see
  requests coming _from_ an ephemeral port but all replies going _to_ 11000.
  v2.12 and v2.14 have an off-by-one bug in the exhaustion check: it compares
  the last port number _tried_ against 11051, regardless of whether that bind
  actually succeeded, so a successful bind on port 11051 specifically is
  misreported as "every port failed" and the app gives up with no message. v2.15
  fixes this (it records success and breaks out of the loop as soon as a bind
  succeeds) and adds a message, absent from earlier versions, for a genuine
  exhaustion: `Unable to open a network\nport (11000-11051)`.
- A simple client can just bind **one** socket to `0.0.0.0:11000`, advertise
  `11000` in discovery, and use that socket for both send and receive.
- Because the controller has a single reply target, **do not run two clients (or
  Masso Link + a client) at once** — they will steal each other's replies.
- Masso Link (every version) discards any received datagram whose source IP is
  not the controller it is connected to, before even checking the CRC. v2.15
  additionally discards any datagram shorter than 5 bytes or longer than 1501
  bytes, and bounds-checks every field it reads against the datagram's actual
  length; v2.12/v2.14 read fixed offsets and, on an unexpectedly short reply,
  see whatever bytes a previous, longer packet left in the shared buffer.

---

## 2. Framing

Every datagram, in both directions:

```
+--------+--------+--------+--------+-----------------------------+
| CRC lo | CRC hi |  0x03  |  0x00  | type |      payload ...     |
+--------+--------+--------+--------+-----------------------------+
   byte0    byte1    byte2    byte3   byte4   byte5 ...
   \___ CRC-16 ___/  \___ magic ___/
```

- **Magic**: constant `03 00` at bytes 2–3.
- **Type**: one byte at offset 4 (see §3).
- **CRC**: **CRC-16/XMODEM** — polynomial `0x1021`, init `0x0000`, no
  input/output reflection — computed over **every byte after the CRC field**
  (i.e. from the magic through the end of the padded body). Stored little-endian
  at bytes 0–1.
- **Body padding**: the on-wire body (everything after the 2 CRC bytes) is
  zero-padded so its length is `(L + 3) & ~3`, where `L` is the intended payload
  length indicator. In practice: assemble `magic + type + payload`, then pad
  with `0x00` up to the next 4-byte boundary. Padding is always ≥ the meaningful
  content, and trailing zeros are harmless (strings are NUL-terminated).
- **Scratch buffer**: Masso Link builds every packet in a 1501-byte buffer.
  v2.15 zero-fills the whole buffer before building each non-upload request
  (discovery, config, tool query, the `0x05` step, keepalive); v2.12/v2.14 only
  overwrite the bytes a given packet type sets, so the unwritten tail carries
  whatever the previous packet left there (see §3.4).

Reference CRC (Python):

```python
def crc16_xmodem(data, crc=0x0000):
    for b in data:
        crc ^= b << 8
        for _ in range(8):
            crc = ((crc << 1) ^ 0x1021) if crc & 0x8000 else (crc << 1)
            crc &= 0xFFFF
    return crc          # store as little-endian 2 bytes
```

---

## 3. Packet types

`type` (byte 4) values used by the app:

| Type   | Direction | Name                         | Reply                            |
| ------ | --------- | ---------------------------- | -------------------------------- |
| `0x02` | →         | Discovery / identity request | 46-byte identity                 |
| `0x03` | →         | Config request (handshake)   | 10-byte serial                   |
| `0x01` | →         | Keepalive / status request   | 270-byte status                  |
| `0x05` | →         | Handshake step (optional)    | advances the app's state machine |
| `0x08` | →         | Tool-table query             | 38-byte tool record              |
| `0x0A` | →         | Upload — start               | 10-byte ACK                      |
| `0x0B` | →         | Upload — data chunk          | 10-byte ACK                      |

Replies from the controller reuse the same `type` byte and are distinguished by
**(length, type)**.

### 3.1 Discovery — `0x02`

Request payload (5 bytes): **reply port (u16 LE)** + `00 00 00`.

```
[crc] 03 00 02 | F8 2A | 00 00 00           ; F8 2A = 0x2AF8 = port 11000
```

Reply (46 bytes): identity / version.

```
[crc] 03 00 02 | serial (u32 LE) | field2 (u32 LE)  | "5-Axis v5.13" 00 ...
 byte:  2  3 4    5   6   7   8     9  10  11  12     13 ... 41
```

- bytes 5–8: the controller's **serial number** (u32 LE); Masso Link compares it
  against the number the operator typed when connecting by serial number (§7).
- bytes 9–12: a second 32-bit field (u32 LE). Masso Link reads it, but no use
  for it was found; its meaning is unknown. On the captured firmware byte 12 is
  `0x40`.
- bytes 13–41: the ASCII, NUL-terminated **version string** (e.g.
  `5-Axis v5.13`). Masso Link reads it from this fixed offset and never past
  byte 41.

**Tool-table ceiling.** Masso Link caps the tool-query loop (§3.4) at 32 when
the serial is ≤ 5000; otherwise at 100 when the version string contains `Lathe`
(a case-insensitive substring match in v2.15; v2.12/v2.14 compare the start of
the string against the literal `Lathe`); otherwise at 118.

### 3.2 Config — `0x03`

Request payload (9 bytes): a local timestamp
`hour, minute, second, day, month, year%100, 0, 0, 0`. **The controller ignores
these bytes** — zeros work equally well; Masso Link sends the wall clock.

```
[crc] 03 00 03 | 0E 07 1E 18 08 1A 00 00 00   ; 14:07:30, day 24, month 08, year 26
```

Reply (10 bytes): `[crc] 03 00 03 | SS SS | 00 00 0A` — bytes 5–6 echo the
serial.

### 3.3 Keepalive / status — `0x01`

Request payload (5 bytes): `hour, minute, second, day, month` (ignored by the
controller). Masso Link sends this once per second; each one elicits a status
packet.

```
[crc] 03 00 01 | 0E 07 1E 18 08
```

Reply: the **270-byte status packet** (§4).

### 3.4 Tool query — `0x08`

Request payload: `index (1 byte, 1..N)` + 4 unspecified bytes. No version of
Masso Link writes bytes 6–9 of this request: they are whatever the previous
packet left in the shared buffer (§2), so their value varies by capture (a v2.12
capture showed `22 2C 1C 0B`), and v2.15 sends zeros. Nothing reads them.

```
[crc] 03 00 08 | 01 | 00 00 00 00            ; query tool #1
```

Reply (38 bytes): `[crc] 03 00 08 | index | name... 00` — byte 5 is the tool
index, bytes 6.. are the NUL-terminated tool name. Masso Link iterates indices
1..118 (mill/router), 1..100 (lathe), or 1..32 for a serial ≤ 5000 (§3.1), and
stops at the first empty name. (On a machine with no named tools, replies carry
empty names.) Its receiver thread sends each next query itself, immediately
after storing a reply.

### 3.5 Upload — `0x0A` (start) and `0x0B` (data)

See §5.

---

## 4. Status packet (270 bytes, type `0x01`)

Offsets are from the start of the datagram.

|    Offset | Size | Field                 | Notes                                                                                                                                                                                                            |
| --------: | ---: | --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
|       0–1 |    2 | CRC                   |                                                                                                                                                                                                                  |
|       2–3 |    2 | magic `03 00`         |                                                                                                                                                                                                                  |
|         4 |    1 | type `0x01`           |                                                                                                                                                                                                                  |
|     **5** |    1 | **progress %**        | 0–100; job completion                                                                                                                                                                                            |
|     **6** |    1 | **run state**         | `0x00` = stopped / idle / feed-hold / e-stop; `0x02` = running                                                                                                                                                   |
|         7 |    1 | `0xFF`                | constant separator                                                                                                                                                                                               |
|  **8–11** |    4 | **job count**         | lifetime jobs counter (u32 LE)                                                                                                                                                                                   |
|    **12** |    1 | **user-prompt flag**  | `0x01` = normal; `0x00` = paused waiting for the operator (manual tool change, `M0`/`M1`, cycle-start). Masso Link inverts this bit internally before using it; the wire value and meaning above are unaffected. |
| **13–16** |    4 | **line number**       | current program line (u32 LE)                                                                                                                                                                                    |
| **17–49** |  ≤33 | **current file name** | NUL-terminated ASCII, max 33 characters (empty when idle); Masso Link never reads past byte 49                                                                                                                   |
|     …–269 |    — | reserved              | `0x00` when idle; holds additional run-time data on a busy machine                                                                                                                                               |

The app renders the machine-state text and the alarm banners it shows —
`Machining`, `Machine Stopped`, `Change Tool`, `SPINDLE ALARM`,
`X/Y/Z/A/B MOTOR ALARM`, `X/Y/Z/A/B HARD LIMIT ALARM`, `SPINDLE COOLANT ALARM`,
`LUBRICANT LOW ALARM`, `TORCH HIT ALARM` — from the run-state (byte 6) and
user-prompt (byte 12) fields. `Jobs` = byte 8–11, progress ring = byte 5.

**Feed-hold vs. e-stop** are not directly distinguishable: both set byte 6 to
`0x00` while byte 5 (progress) freezes. A feed hold can be _inferred_ when the
line number (bytes 13–16) stops advancing for ≥ ~1.5 s while the state had been
running.

---

## 5. File upload

Uploads write a file to the controller's USB flash drive, at the path carried in
the start packet's path field (§5.1) — the root by default, or a subdirectory
for a folder drop (v2.15; §5.1). The file is stored only; it is not executed.

**Filename rules** (enforced by the app, not the controller):

- ≤ **15 characters**, ASCII. No length check on the file name was found in any
  Masso Link binary, v2.12 through v2.15; the limit is a documented assumption,
  not yet tested against real hardware with a longer name.
- Path separator is backslash `\` (forward slash is not accepted).
- The app only offers files whose extension is one of
  `.nc .txt .cnc .tap .eia .htg .wiz .gcode .ngc`.

### 5.1 Start — `0x0A`

Payload:

```
[crc] 03 00 0A | size(u32 LE) | 00 00 | pathlen(1) | path... | 00 | name... | 00 | 00 00 00 | pad
                 \__ 4 bytes _/  \_2_/                                          \_reserved_/
```

The packet's length is **not fixed**. Masso Link computes the un-padded payload
length as `pathlen + namelen + 15` — the 15 covers the 4-byte size, the 2
reserved bytes, the 1-byte pathlen, the NUL after the path, the NUL after the
name, and 3 further reserved zero bytes after that NUL whose purpose is unknown
and which no version of Masso Link reads back — then pads that to a 4-byte
multiple exactly as §2 describes, then adds the 2-byte CRC:

```
total_bytes = 2 + (((pathlen + namelen + 15) + 3) & ~3)
```

The formula is the same in every version: `pathlen=1, namelen=9` (the
`CLTEST.NC` example below) gives 30 bytes, a 5-character name 26 bytes, a
15-character name 34 bytes.

For a plain root upload the path is a single backslash (`pathlen=1`, path byte
`5C`), unless a base-path setting (normally empty) is configured. Captured
example for an 87-byte file named `CLTEST.NC`:

```
06 39 03 00 0A 57 00 00 00 00 00 01 5C 00 43 4C 54 45 53 54 2E 4E 43 00 00 00 00 00 00 00
\crc/ \magic/ ty \_ size=0x57 _/ \__/ pl \/ \/ \___ "CLTEST.NC" ______/ \0/ \___ 6 zero bytes ___/
```

(3 of those trailing zero bytes are the reserved field above; the other 3 are
the generic §2 padding bringing 25 bytes up to the next 4-byte multiple, 28.)

**Folder uploads (v2.15).** Dropping a folder queues each file inside it and
sends every one with this same packet, with the path field set to that file's
directory _relative to the dropped folder_ — for example `JOBS` for a file
directly inside a dropped folder named `JOBS`, or `JOBS\SUB` one level deeper:

- the relative path is seeded with the dropped folder's own name, then each
  deeper directory name is appended with a single backslash — never a leading or
  trailing backslash;
- the name field carries only the bare file name; the relative path never
  includes it;
- the walk is capped at 16 levels of recursion and 500 queued entries total;
- a file or directory whose name contains a TAB character is silently skipped
  (v2.15 uses TAB as a delimiter in its internal queue);
- a client-side check rejects `pathlen + namelen + 19 > 1501` with a generic
  local error; both fields are Pascal short strings of at most 255 bytes, so it
  cannot fire.

No packet exists for creating a directory on the controller. Whether the
controller creates missing directories itself, or requires them to already
exist, is unverified.

**Start ACK** (10 bytes, type `0x0A`): byte 5 is the result:

| byte 5 | Meaning                                                                                                                                          |
| -----: | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `0x00` | OK — USB present and writable; proceed to send chunks                                                                                            |
| `0xE9` | **No USB flash drive connected**                                                                                                                 |
| `0xF7` | success, but **only** in reply to a start request the client has already resent (v2.15; §5.3); in reply to a first attempt it is a generic error |
|  other | generic transfer error                                                                                                                           |

The `0xF7` case only arises for a client that retransmits the start packet
(v2.15; §5.3): a `0xF7` reply to a resent request is read as "already started",
the first attempt having succeeded. v2.12 never resends the start packet and
treats `0xF7` like any other error.

### 5.2 Data chunk — `0x0B`

Payload: `chunk index (u32 LE, 0-based)` + `chunk length (u32 LE)` + `data`.
Maximum data per chunk is **1422 bytes** (`0x58E`). The final chunk carries only
the remaining bytes (it is _not_ padded up to 1422 in the length field).

```
[crc] 03 00 0B | index(u32) | length(u32) | data[length] | pad
```

**Chunk ACK** (10 bytes, type `0x0B`): byte 5 is the result; bytes 6–9 echo the
number of chunks the controller has accepted so far.

| byte 5 | Meaning                                                       |
| -----: | ------------------------------------------------------------- |
| `0x00` | OK                                                            |
| `0x01` | **Unable to write file to USB**                               |
| `0x02` | **Canceled by the user on the Masso** (payload spells `USER`) |

### 5.3 Sequence and timing

```
client                          controller
  |-- 0x0A start (size,name) ------>|
  |<-- 0x0A ACK (byte5=00/0xF7) ----|      byte5 not 00, and not a post-retry 0xF7  -> abort with the error above
  |-- 0x0B chunk 0 (<=1422B) ------>|
  |<-- 0x0B ACK (byte5=00) ---------|
  |-- 0x0B chunk 1 ---------------->|
  |<-- 0x0B ACK --------------------|
  |            ...                  |
  |-- 0x0B chunk N (remainder) ---->|
  |<-- 0x0B ACK --------------------|      done
```

- The controller learns the total size from the start packet and writes exactly
  that many bytes, so any padding in the last chunk is discarded.
- The chunk ACK's accepted-chunk count (§5.2) only ever moves the client's
  "accepted so far" pointer forward, clamped to the number of chunks sent.

**v2.12** sends the start packet once and busy-waits (no sleep between polls) up
to 5 s for its ACK, giving up with no reply. It then sends one chunk at a time,
waiting for that chunk's ACK before sending the next, retransmitting the same
chunk after a fixed ~100 ms with no reply, and aborting the whole transfer —
`ERROR: No response from MASSO` — after 15 s with no ACK activity of any kind.

**v2.15** changes both the start handshake and the chunk loop:

- it resends the start packet roughly once per second (via a real sleep between
  checks, not a busy spin) until it gets an ACK or 5 s pass, whichever comes
  first — see the `0xF7` case in §5.1;
- it keeps up to **2 chunks in flight** rather than 1. The window opens from 1
  to 2 after 3 consecutive chunks are acknowledged with no retransmission, and
  drops back to 1 the moment any chunk needs a retransmission;
- instead of a fixed ~100 ms retransmit interval, it tracks a smoothed
  round-trip estimate, seeded at 60 ms and updated on every acknowledgment of a
  chunk that was never itself retransmitted: `SRTT := (3 × SRTT + sample) / 4`.
  The retransmit timeout is `2 × SRTT`, clamped to 40–250 ms; whenever the
  oldest unacknowledged chunk has been outstanding longer than that, every chunk
  currently in flight is resent;
- it still gives up after 15 s with no ACK activity of any kind.

**Compatible clients.** mink-lasso implements the v2.15 behavior above: a
variable-length start packet (§5.1), a zeroed tool-query trailer (§3.4), the
32-bit serial-comparison field (§3.1), the 33-byte status file-name limit (§4),
start retransmission at 1 s up to a 5 s budget with the `0xF7` handling (§5.1),
a 2-chunk window with the adaptive retransmit timeout, the 15 s overall give-up,
source-IP filtering of status/tool/ACK replies (§1), and the `0x0C` notification
(§5.5) after a failed but acknowledged transfer.

### 5.4 Controller result → message mapping (from the app)

| Condition                                                                          | Message shown by Masso Link                  |
| ---------------------------------------------------------------------------------- | -------------------------------------------- |
| no ACK / timeout                                                                   | `ERROR: No response from MASSO`              |
| start ACK byte5 = `0xE9`                                                           | `No USB Flash drive connected to MASSO`      |
| start ACK byte5 = `0xF7`, after ≥1 start retransmit (v2.15)                        | success — treated the same as byte5 = `0x00` |
| start ACK byte5 = other (not `0x00`, and not a post-retry `0xF7`)                  | `An error occurred while transferring file`  |
| chunk ACK byte5 = `0x02` (`USER`)                                                  | `File transfer canceled by user on MASSO`    |
| chunk ACK byte5 = `0x01`                                                           | `Unable to write file to USB`                |
| local cancel — clicking Send again mid-transfer (v2.15 only; no wire ACK involved) | `File transfer canceled`                     |
| all chunks ACKed                                                                   | success (`File sent`)                        |

### 5.5 Post-transfer signal — `0x0C` (v2.15)

```
[crc_lo][crc_hi] 03 00 0C 00 00 00 00 00        ; 10 bytes total, no payload
```

v2.15 sends this 10-byte, payload-free packet three times, 20 ms apart, with no
reply expected; no version's receiver ever tests an incoming packet's type
against `0x0C`, and v2.12/v2.14 never send it.

It is sent when the start request got an ACK (the upload actually began) but the
transfer did **not** end cleanly — a controller error result, a cancel on the
Masso, a local cancel, or the 15 s give-up above. A transfer that completes
cleanly never sends it.

Its effect on the controller is unknown; the most plausible reading, since it
names no file or path, is an abort/cleanup notification. mink-lasso sends it
under the same condition. Whether the controller needs it has not been tested.

### 5.6 Multiple files and folder drops (v2.15)

Dropping several files, or a folder, queues them and sends them one at a time,
each with its own complete start/chunks/ACK sequence (§5.1–§5.4), under a
"Sending N of M" caption. Any error on any file stops the whole remaining queue;
only a clean success (all chunks acknowledged, not canceled) advances to the
next file. Clicking the send control again mid-transfer cancels locally (§5.4);
v2.12/v2.14 ignore a click while a transfer is in progress.

---

## 6. Full handshake, end to end

```
client                                   controller
  |-- 0x02 discovery (reply-port) ---------->|
  |<-- 0x02 identity (serial, "5-Axis vX") --|
  |-- 0x03 config (timestamp) -------------->|
  |<-- 0x03 serial -------------------------|
  |-- 0x01 keepalive  (1 Hz) --------------->|
  |<-- 0x01 status (270B) ------------------|   ... repeats at ~1 Hz
```

(The app also uses a `0x05` step internally between config and steady-state; it
is not required to read status or upload files. The ~1 Hz keepalive rate is what
the v2.12 capture showed; the connection dispatcher that sends it runs on a 200
ms timer, `tmr_Get_MASSO_Info`, in every version, and what throttles it to ~1 Hz
was not identified. v2.12/v2.14 send the keepalive packet **twice** per
dispatcher tick while connected — a conditional send gated on a "still waiting
for a reply" flag, immediately followed by an unconditional one; v2.15 sends it
once.)

---

## 7. Discovering controllers and connecting by serial number

Broadcast a discovery packet to `255.255.255.255:65535`; each controller answers
with its 46-byte identity (§3.1). v2.12 connects by a literal IP address only.

**Connecting by serial number (v2.14+).** Starting at v2.14, Masso Link's
connect dialog accepts either an IP address or, in the same field, something
like `G3-31578` (hint text: "Serial Number or IP address"). It resolves a serial
number before handing anything to the main connection state machine:

1. it splits the typed text on `-` and parses the trailing segment as an
   integer;
2. it opens a second, throwaway UDP socket bound to the first free port in
   **11052–11057** (immediately above the normal 11000–11051 receive range; the
   two never collide);
3. it broadcasts an ordinary discovery packet (§3.1), advertising that throwaway
   port as the reply port, to up to four targets: the last known controller IP,
   that IP's subnet broadcast address, the local subnet's broadcast address, and
   `255.255.255.255`;
4. it repeats that broadcast for up to **4 rounds within a 2.5 s deadline**,
   with a 250 ms receive timeout on each attempt;
5. it accepts the **first** identity reply whose bytes 5–8 (§3.1) equal the
   parsed number, and adopts that reply's **source IP** — not anything in the
   reply's payload — as the resolved address;
6. on failure it shows `Host <name> not found or invalid IP address`.

Once resolved (or once a literal IP is typed), the main connection state machine
broadcasts an ordinary discovery packet every tick of its 200 ms timer while it
has no confirmed address, exactly like a plain LAN scan. Every 50 ticks (10 s)
it checks whether resolution has completed; once it has, it tears down the
discovery socket, opens a new one `Connect()`-ed directly to the resolved IP on
port 65535, and shows `Trying to connect to\nIP: <ip>`.

Both of these are broadcasts that retarget **every** controller that hears them
— exactly the hazard §1 describes. Connecting by serial number adds a filter on
top of the same broadcast-and-match mechanism; mink-lasso's own broadcast
discovery, which matches bytes 5–8 of each identity reply against the configured
serial and then addresses that reply's source IP directly, is equivalent.

---

## 8. Connection liveness and reconnection

Masso Link resets a countdown to a full budget every time any datagram from the
controller passes the CRC check (§2), regardless of packet type — not only on a
status or identity reply.

- **v2.12/v2.14**: the budget is **10 s**. On expiry it runs a single,
  disruptive path: it hides the status controls, tears the socket down and
  recreates it, and returns to the discovery state.
- **v2.15**: the budget is **5 s**. The first two expiries only drop the
  connection state back to discovery for a fresh 5 s window, which sends the
  ordinary discovery broadcast of §7 with no UI change; the **third**
  consecutive expiry (up to ~15 s of silence) runs the same disruptive path as
  v2.12, after first repeating the serial resolution of §7 if the connection was
  made by serial number.

So v2.15 takes longer than v2.12 to reach the disruptive reconnect (~15 s vs. 10
s), but absorbs a stall of a few seconds by re-broadcasting discovery instead of
tearing the connection down.

---

## 9. Differences vs. earlier community reverse-engineering

This spec corrects and extends the prior community work
(`andrewpc/masso-link-protocol-client`, tested on a Lathe running v5.09):

- The status **line number is a `u32`** at bytes 13–16 (not a single byte).
- The discovery request's `F8 2A` bytes are the client's **reply port** (11000),
  not a magic constant — set them to whatever port you bind.
- The body is **zero-padded to a 4-byte multiple** via `(len+3) & ~3`.
- Confirmed the upload chunk cap of **1422 bytes** and the ACK result-code bytes
  directly from the v2.12 binary.
- Everything here is confirmed on **mill/router-class (5-Axis) firmware v5.13**,
  complementing the earlier lathe-only testing.

---

## 10. Notes

- The controller ignores the timestamp bytes in the config/keepalive requests,
  so the protocol carries no authentication — anything on the LAN that can reach
  UDP 65535 can read status and write files to the USB drive. Nothing found in
  v2.14/v2.15 changes this.
- **Threading (not wire-visible).** v2.12 builds every packet in one
  process-wide buffer with no locking, shared by the send and receive threads,
  so a keepalive built while the other thread is mid-chunk can corrupt either
  packet before it goes out. v2.15 gives each thread its own buffer, decodes
  both ACK types in the receiver thread, and adds two critical sections — one
  around the shared ACK-result flags, one around the socket send call. This is
  the likeliest substance of the "more stable and reliable communication"
  release note.
- **macOS.** The v2.15 Apple Silicon build is a native arm64 binary with the
  same CRC, framing, and object/socket layout as the Linux and Windows builds;
  nothing about the wire format is architecture-specific.
- Verified toolchain: Masso Link v2.12, controller firmware v5.13, tshark 4.2.6,
  Ghidra 12.1.3 for everything tagged v2.12 above; Capstone 5.0.7 and angr
  10.0.0 over the Linux ELF, Windows PE, and macOS Apple Silicon builds of Masso
  Link v2.14 and v2.15.0 for everything tagged v2.14/v2.15.

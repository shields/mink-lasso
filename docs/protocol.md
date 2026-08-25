# MASSO Link UDP Protocol

Full wire-format documentation for the protocol spoken between the **MASSO Link**
desktop application and a **MASSO G3 / G3-Touch** CNC controller fitted with the
Wi-Fi / Ethernet module.

Reverse-engineered from **MASSO Link v2.12** (Hind Technology Australia, a 64-bit
Free Pascal / Lazarus application using the Synapse `TUDPBlockSocket`). Every
statement below is backed by one or both of:

* **Static analysis** — Ghidra decompilation of the sender thread
  (`Send_NC_File_Thread_Class.Execute`), the receiver thread
  (`UDP_Server_Thread_Class.Execute`), the send routine, the CRC routine, and
  the status/timer UI code.
* **Live capture** — packets recorded with tshark against a real controller
  (a 5-Axis machine running firmware **v5.13**) plus an independent Python
  re-implementation ([`masso_link.py`](masso_link.py)) that connects, reads
  status/tools, and uploads a file — verified byte-for-byte against the app.

> This is unofficial documentation. It is not affiliated with or endorsed by
> MASSO. Uploading a file only writes it to the controller's USB drive; it does
> **not** run anything. Only one client may talk to a controller at a time.

---

## 1. Transport and connection model

| Property | Value |
|---|---|
| Protocol | UDP |
| Controller listening port | **65535** (all client → controller packets go here) |
| Client listening port | **11000** by default (see below) |
| Byte order | little-endian for all multi-byte integers |

**How the controller decides where to reply.** The client does *not* rely on the
controller replying to the request's source port. Instead, the **discovery
packet carries the port the client wants replies on** (bytes 5–6, little-endian).
The controller stores `<client-IP, that-port>` and sends *every* subsequent reply
and status broadcast to it.

* MASSO Link binds the first free UDP port in the range **11000–11050** for
  receiving, and advertises that port in the discovery packet. It sends its
  requests from a *separate* ephemeral socket. In a capture you therefore see
  requests coming *from* an ephemeral port but all replies going *to* 11000.
* A simple client can just bind **one** socket to `0.0.0.0:11000`, advertise
  `11000` in discovery, and use that socket for both send and receive.
* Because the controller has a single reply target, **do not run two clients (or
  MASSO Link + a client) at once** — they will steal each other's replies.

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

* **Magic**: constant `03 00` at bytes 2–3.
* **Type**: one byte at offset 4 (see §3).
* **CRC**: **CRC-16/XMODEM** — polynomial `0x1021`, init `0x0000`, no input/output
  reflection — computed over **every byte after the CRC field** (i.e. from the
  magic through the end of the padded body). Stored little-endian at bytes 0–1.
* **Body padding**: the on-wire body (everything after the 2 CRC bytes) is
  zero-padded so its length is `(L + 3) & ~3`, where `L` is the intended payload
  length indicator. In practice: assemble `magic + type + payload`, then pad with
  `0x00` up to the next 4-byte boundary. Padding is always ≥ the meaningful
  content, and trailing zeros are harmless (strings are NUL-terminated).

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

| Type | Direction | Name | Reply |
|------|-----------|------|-------|
| `0x02` | → | Discovery / identity request | 46-byte identity |
| `0x03` | → | Config request (handshake) | 10-byte serial |
| `0x01` | → | Keepalive / status request | 270-byte status |
| `0x05` | → | Handshake step (optional) | advances the app's state machine |
| `0x08` | → | Tool-table query | 38-byte tool record |
| `0x0A` | → | Upload — start | 10-byte ACK |
| `0x0B` | → | Upload — data chunk | 10-byte ACK |

Replies from the controller reuse the same `type` byte and are distinguished by
**(length, type)**.

### 3.1 Discovery — `0x02`

Request payload (5 bytes): **reply port (u16 LE)** + `00 00 00`.

```
[crc] 03 00 02 | F8 2A | 00 00 00           ; F8 2A = 0x2AF8 = port 11000
```

Reply (46 bytes): identity / version.

```
[crc] 03 00 02 | SS SS | ...  | 40 | "5-Axis v5.13" 00 ...
 byte:  2  3 4    5  6            n
```

* bytes 5–6: controller serial number (u16 LE).
* a `0x40` (`@`) byte marks the start of the ASCII **version string** (e.g.
  `5-Axis v5.13`), which is NUL-terminated. (The string begins at byte 13 on the
  captured firmware; locating it via the `@` marker is version-robust.)

### 3.2 Config — `0x03`

Request payload (9 bytes): a local timestamp
`hour, minute, second, day, month, year%100, 0, 0, 0`.
**The controller ignores these bytes** — zeros work equally well; MASSO Link
sends the wall clock.

```
[crc] 03 00 03 | 0E 07 1E 18 08 1A 00 00 00   ; 14:07:30, day 24, month 08, year 26
```

Reply (10 bytes): `[crc] 03 00 03 | SS SS | 00 00 0A` — bytes 5–6 echo the serial.

### 3.3 Keepalive / status — `0x01`

Request payload (5 bytes): `hour, minute, second, day, month` (ignored by the
controller). MASSO Link sends this once per second; each one elicits a status
packet.

```
[crc] 03 00 01 | 0E 07 1E 18 08
```

Reply: the **270-byte status packet** (§4).

### 3.4 Tool query — `0x08`

Request payload: `index (1 byte, 1..N)` + `22 2C 1C 0B` (constant; only the index
matters).

```
[crc] 03 00 08 | 01 | 22 2C 1C 0B            ; query tool #1
```

Reply (38 bytes): `[crc] 03 00 08 | index | name... 00`
— byte 5 is the tool index, bytes 6.. are the NUL-terminated tool name.
The app iterates indices 1..118 (mill/router) or 1..100 (lathe) and stops at the
first empty name. (On a machine with no named tools, replies carry empty names.)

### 3.5 Upload — `0x0A` (start) and `0x0B` (data)

See §5.

---

## 4. Status packet (270 bytes, type `0x01`)

Offsets are from the start of the datagram.

| Offset | Size | Field | Notes |
|-------:|-----:|-------|-------|
| 0–1 | 2 | CRC | |
| 2–3 | 2 | magic `03 00` | |
| 4 | 1 | type `0x01` | |
| **5** | 1 | **progress %** | 0–100; job completion |
| **6** | 1 | **run state** | `0x00` = stopped / idle / feed-hold / e-stop; `0x02` = running |
| 7 | 1 | `0xFF` | constant separator |
| **8–11** | 4 | **job count** | lifetime jobs counter (u32 LE) |
| **12** | 1 | **user-prompt flag** | `0x01` = normal; `0x00` = paused waiting for the operator (manual tool change, `M0`/`M1`, cycle-start) |
| **13–16** | 4 | **line number** | current program line (u32 LE) |
| **17–…** | var | **current file name** | NUL-terminated ASCII (empty when idle) |
| …–269 | — | reserved | `0x00` when idle; holds additional run-time data on a busy machine |

The app renders the machine-state text and the alarm banners it shows —
`Machining`, `Machine Stopped`, `Change Tool`, `SPINDLE ALARM`,
`X/Y/Z/A/B MOTOR ALARM`, `X/Y/Z/A/B HARD LIMIT ALARM`, `SPINDLE COOLANT ALARM`,
`LUBRICANT LOW ALARM`, `TORCH HIT ALARM` — from the run-state (byte 6) and
user-prompt (byte 12) fields. `Jobs` = byte 8–11, progress ring = byte 5.

**Feed-hold vs. e-stop** are not directly distinguishable: both set byte 6 to
`0x00` while byte 5 (progress) freezes. A feed hold can be *inferred* when the
line number (bytes 13–16) stops advancing for ≥ ~1.5 s while the state had been
running.

---

## 5. File upload

Uploads write a file to the **root of the controller's USB flash drive**
(subdirectories are possible with a `\`-separated path, but the directory must
already exist on the drive — the protocol does not create directories). The file
is stored only; it is not executed.

**Filename rules** (enforced by the app, not the controller):
* ≤ **15 characters**, ASCII.
* Path separator is backslash `\` (forward slash is not accepted).
* The app only offers files whose extension is one of
  `.nc .txt .cnc .tap .eia .htg .wiz .gcode .ngc`.

### 5.1 Start — `0x0A`

Payload:

```
[crc] 03 00 0A | size(u32 LE) | 00 00 | pathlen(1) | path... | 00 | name... | 00 | pad
                 \__ 4 bytes _/  \_2_/   \___ e.g. 01 5C ("\") 00 ___/  \_ filename _/
```

For a plain root upload the path is a single backslash, so bytes 9–13 are
`00 00 01 5C 00`, immediately followed by the filename and its NUL. Example for a
87-byte file named `CLTEST.NC`:

```
[crc] 03 00 0A 57 00 00 00 00 00 01 5C 00 43 4C 54 45 53 54 2E 4E 43 00 00 00 00 00 00
      \magic/ ty \_ size=0x57 _/ \__/ pl \/ \/ \___ "CLTEST.NC" ______/ \0 \_ pad _/
```

**Start ACK** (10 bytes, type `0x0A`): byte 5 is the result:

| byte 5 | Meaning |
|-------:|---------|
| `0x00` | OK — USB present and writable; proceed to send chunks |
| `0xE9` | **No USB flash drive connected** |
| other  | generic transfer error |

### 5.2 Data chunk — `0x0B`

Payload: `chunk index (u32 LE, 0-based)` + `chunk length (u32 LE)` + `data`.
Maximum data per chunk is **1422 bytes** (`0x58E`). The final chunk carries only
the remaining bytes (it is *not* padded up to 1422 in the length field).

```
[crc] 03 00 0B | index(u32) | length(u32) | data[length] | pad
```

**Chunk ACK** (10 bytes, type `0x0B`): byte 5 is the result; bytes 6–9 echo the
number of chunks the controller has accepted so far.

| byte 5 | Meaning |
|-------:|---------|
| `0x00` | OK |
| `0x01` | **Unable to write file to USB** |
| `0x02` | **Canceled by the user on the MASSO** (payload spells `USER`) |

### 5.3 Sequence and timing

```
client                          controller
  |-- 0x0A start (size,name) ------>|
  |<-- 0x0A ACK (byte5=00) ---------|      byte5!=0  -> abort with the error above
  |-- 0x0B chunk 0 (<=1422B) ------>|
  |<-- 0x0B ACK (byte5=00) ---------|
  |-- 0x0B chunk 1 ---------------->|
  |<-- 0x0B ACK --------------------|
  |            ...                  |
  |-- 0x0B chunk N (remainder) ---->|
  |<-- 0x0B ACK --------------------|      done
```

* The controller learns the total size from the start packet and writes exactly
  that many bytes, so any padding in the last chunk is discarded.
* MASSO Link waits for each ACK, **retransmits** a chunk if no ACK arrives within
  ~100 ms, and **aborts the whole transfer after 15 s** without progress
  (surfacing `ERROR: No response from MASSO`).

### 5.4 Controller result → message mapping (from the app)

| Condition | Message shown by MASSO Link |
|---|---|
| no ACK / timeout | `ERROR: No response from MASSO` |
| start ACK byte5 = `0xE9` | `No USB Flash drive connected to MASSO` |
| start ACK byte5 = other ≠ 0 | `An error occurred while transferring file` |
| chunk ACK byte5 = `0x02` (`USER`) | `File transfer canceled by user on MASSO` |
| chunk ACK byte5 = `0x01` | `Unable to write file to USB` |
| all chunks ACKed | success (`File sent`) |

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
is not required to read status or upload files.)

---

## 7. Discovering controllers on the LAN

Broadcast a discovery packet to `255.255.255.255:65535`; each controller answers
with its 46-byte identity (serial + version). MASSO Link **v2.14+** with firmware
**v5.13+** additionally supports connecting by serial number (`G3-xxxxx`) instead
of a fixed IP; v2.12 (documented here) connects by IP address.

---

## 8. Differences vs. earlier community reverse-engineering

This spec corrects and extends the prior community work
(`andrewpc/masso-link-protocol-client`, tested on a Lathe running v5.09):

* The status **line number is a `u32`** at bytes 13–16 (not a single byte).
* The discovery request's `F8 2A` bytes are the client's **reply port** (11000),
  not a magic constant — set them to whatever port you bind.
* The body is **zero-padded to a 4-byte multiple** via `(len+3) & ~3`.
* Confirmed the upload chunk cap of **1422 bytes** and the ACK result-code bytes
  directly from the v2.12 binary.
* Everything here is confirmed on **mill/router-class (5-Axis) firmware v5.13**,
  complementing the earlier lathe-only testing.

---

## 9. Notes

* `MASSO_Link.dat` is the app's settings file (machine name, mapped folder, last
  IP). It is a Lazarus-serialized binary blob and is irrelevant to the protocol.
* The controller ignores the timestamp bytes in the config/keepalive requests, so
  the protocol carries no authentication — anything on the LAN that can reach UDP
  65535 can read status and write files to the USB drive.
* Verified toolchain: MASSO Link v2.12, controller firmware v5.13, tshark 4.2.6,
  Ghidra 12.1.3. See [`masso_link.py`](masso_link.py) for a working client.

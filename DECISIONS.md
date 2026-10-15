# Decisions and API interpretations

The reference is FlexLib v4.3.15.42017, the public zip. File and line numbers refer to that source.

## Meter protocol

| Topic | Interpretation | Source |
|---|---|---|
| Meter list | `sub meter all` makes the radio send `meter` status lines; `meter list` returns the same text as a reply | Radio.cs 2339, 6773–6782 |
| Status format | `<i>.key=value` tokens separated by `#` (desc may contain spaces); keys src, num, nam, low, hi, desc, unit, fps | Radio.cs `ParseMeterStatus` 6784–7000 |
| Removal | `meter <i> removed` | Radio.cs 6791–6844 |
| `num` | Decimal slice index for SLC; `0x%08X` client handle for amplifier/tuner meters | Radio.cs 6908–6934 |
| Duplicate names | The newest (highest index) meter of a name is the one the radio streams | Meter.cs `FindNewestByName` |
| Values transport | VITA-49 type ExtDataWithStream, packet class 0x8002 | VitaFlex.cs 24; Radio.cs 16077–16106 |
| Packet payload | Repeated big-endian uint16 id + int16 raw | VitaMeterPacket.cs |
| Scaling | dB/dBm/dBFS/SWR ÷128; Volts/Amps ÷256 (÷1024 before fw 1.11); degF/degC ÷64; others raw | Meter.cs `UpdateValue` |
| UDP port | LAN clients send `client udpport <port>`; FlexLib uses 4991, and we walk up if it's taken (as FlexTCI does) | Radio.cs 2384–2389 |
| Station bind | `client bind client_id=<id>` | Radio.cs 15399–15404 |
| Discovery | UDP 4992 broadcast, key=value payload; parser reused from FlexTCI | Discovery.cs 30 |

## Choices (2026-10-15)

- Go + Wails, a single Windows exe, the same stack as FlexTCI. LAN only; no SmartLink.
- Non-GUI client that binds to a chosen SmartSDR station. Radio-wide meters are always shown; other stations' slice meters only on request.
- Receive only (see README for the exact command list).
- Selections are keyed by `SLC/<letter>/<name>` or `<src>/<name>`, because meter indices change between sessions. `num` is left out because it is a session-specific client handle for external meters (see findings).
- TX/amplifier power meters that arrive in dBm are shown in watts (W = 10^((dBm−30)/10)). Their range is shown in watts too.
- Values are pushed at 15 Hz and only for the selected meters. A meter with no packet for 3 s shows "—".
- SWR defaults to a 1–3 display range because the radio reports hi=999. Any range can be overridden per meter.
- Firmware before 1.11 (volts ÷1024) is not handled.

## Findings: FLEX-8600 "ANT1-VA3MW", firmware v4.3.16.42034, 2026-10-15

Connected receive-only through `tools/preview`. No GUI client was attached, so there were no slices or slice meters. The radio announced 12 meters:

| idx | src | num | name | unit | low..hi | fps |
|---|---|---|---|---|---|---|
| 1 | COD- | 1 | MICPEAK | dBFS | −150..20 | 40 |
| 2 | COD- | 2 | MIC | dBFS | −150..20 | 20 |
| 3 | TX- | 5 | HWALC | dBFS | −150..20 | 20 |
| 4 | RAD | 334 | +13.8A | Volts | 10.5..15 | 0 |
| 5 | RAD | 300 | PACURRENT | Amps | 0..10 | 0 |
| 6 | RAD | 0 | +13.8B | Volts | 10.5..15 | 0 |
| 7 | RAD | 3 | MAINFAN | RPM | 0..8192 | 0 |
| 8 | TX- | 1 | FWDPWR | dBm | 0..53 | 20 |
| 9 | TX- | 2 | REFPWR | dBm | 0..53 | 20 |
| 10 | TX- | 3 | SWR | SWR | 1..999 | 20 |
| 11 | TX- | 4 | PATEMP | degC | 0..120 | 0 |
| 12 | EXT_WVF | 1176883659 | FreeDV_SNR | dB | −99..99 | 0 |

- TX power is reported in **dBm**, not watts. FlexMeters converts it.
- `num` for `EXT_WVF` is a client handle written in **decimal**, not `0x…` as FlexLib's comment describes for refresh meters. Keys therefore leave out `num`.
- PATEMP is under `TX-`, not `RAD`.
- Meter packets arrive at about 33 per second, idle, as a non-GUI client with no bind, so binding is not needed to receive the radio-wide meters.
- Idle values were plausible: +13.8A 14.0 V, +13.8B 14.6 V, PACURRENT 0.01 A, fan 980 RPM, PA 12 °C, SWR 1.00, HWALC 0 dBFS.
- `client program FlexMeters` → `0x10000002 unknown client program`, even though FlexLib sends `client program <ProgramName>` the same way (Radio.cs 2296–2298). The firmware evidently accepts only names it knows, so FlexMeters no longer sends it.
- The radio also sent `M10000001|Client connected from IP …` on connect.

## Findings: AU-520 "F4-520", firmware v4.3.16.42034, 2026-10-15

Connected receive-only through `tools/preview`, with one GUI client attached: SmartSDR-Maestro, station "Maestro", owning slice A. FlexMeters bound to it automatically. The radio announced 36 meters. Compared with the 8600:

- **Slice meters** (`SLC`, num=`0`, decimal, as Radio.cs 6908–6934 describes): `24kHz`, `ESC`, `LEVEL`, `AGC+`, all dBm −150..20 at 10 fps. LEVEL read about −92 dBm and AGC+ about −35 dBm.
- **PA meters** not on the 8600 list: `PASTAT` (unit **`bits`**, "PA Fault Status", 0..256; read 0), `PAFETQ1TEMP`/`PAFETQ2TEMP` (degC), `PAEFF` (Percent, under `TX-`).
- **Transmit chain:** 15 dBFS stage meters under `TX-` with num=0 (CODEC, TXAGC, SC_MIC, AFTEREQ, SC_FILT_0..2, COMPPEAK, ALC, RM_TX_AGC, TX_AGC, B4RAMP, AFRAMP, POST_P, ATTN_FPGA). These appear only with a GUI client attached; the 8600 had none.
- **`PAN` source:** `NOISE_FLOOR` (dBm −180..20), num = 1073741824 (0x40000000, the panadapter stream id, in decimal). **No values reached FlexMeters in 10 s**, so pan meters appear to stream only to the pan's owner. Two panadapters would give two NOISE_FLOOR meters with the same key; FlexMeters keeps only the newest.
- FWDPWR/REFPWR range is 0..57.8 dBm (about 600 W) here, versus 0..53 on the 8600.
- `bits` is a unit FlexLib's MeterUnits enum does not list (Meter.cs), so FlexLib leaves it raw. FlexMeters shows it as hex, with the bar lit red if any bit is set.
- +13.8A read 11.9 V (10.5..15 range).

## Not yet verified on a real radio

- Amplifier meters (PGXL/Tuner Genius).
- Values while transmitting (dBm → W conversion against SmartSDR's own power display).
- Whether a bound non-GUI client receives meter packets for every meter, or only for some, after `sub meter all`.
- Behaviour with SmartSDR running on the same PC (UDP 4991 taken → FlexMeters uses 4992+).

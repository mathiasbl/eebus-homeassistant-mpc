# EEBus MA-MPC Bridge — Home Assistant Addon

Bridges **EEBus heat pump devices** to Home Assistant using MQTT discovery.  
Uses the **MA-MPC** (Monitoring of Power Consumption) and **EG-LPC** (Limitation of Power Consumption) use cases from the [enbility/eebus-go](https://github.com/enbility/eebus-go) library.

Tested with: **Vaillant VR921** (heat pump gateway).  
Should work with any EEBus device that advertises `HeatPumpAppliance`, `Compressor`, or `Inverter` entity types.

---

## What it does

1. **Discovers** all EEBus devices on the local network continuously via mDNS.
2. **Initiates pairing** with every discovered device automatically.
3. **Waits** for the user to approve the pairing on the device side (e.g. myVaillant app).
4. **Publishes** a Home Assistant MQTT device with sensor entities once paired.
5. **Forwards** measurement events in real time as they arrive from the device.
6. **Re-publishes** discovery automatically when Home Assistant restarts.

---

## Entities created per device

| Entity | Unit | Notes |
|---|---|---|
| Power | W | Total active power — enabled by default |
| Power L1 / L2 / L3 | W | Per-phase — disabled by default |
| Current L1 / L2 / L3 | A | Per-phase — disabled by default |
| Voltage L1 / L2 / L3 | V | Per-phase — disabled by default |
| Frequency | Hz | Grid frequency — disabled by default |

Per-phase entities can be enabled in **Settings → Devices & Services → your device → Enable entity**.

---

## Prerequisites

- Home Assistant with the **Mosquitto broker** addon installed and running.
- Your heat pump must support EEBus (e.g. Vaillant aroTHERM, VR921 gateway).
- The HA host and the heat pump must be on the **same local network segment** (mDNS/multicast must work between them).

---

## Installation

### Option A — Home Assistant Addon repository (recommended)

1. In Home Assistant, go to **Settings → Add-ons → Add-on Store**.
2. Click the **⋮ menu** (top right) → **Repositories**.
3. Add the repository URL and click **Add**.
4. Find **EEBus MA-MPC Bridge** in the store and click **Install**.

### Option B — Manual sideload

1. Copy the `ha-addon/` folder into your HA config directory under `addons/`:
   ```
   /config/addons/eebus-bridge/
   ```
2. In Home Assistant, go to **Settings → Add-ons → Add-on Store** and click **⋮ → Check for updates**.  
   The addon will appear under **Local add-ons**.
3. Click **Install**.

---

## Configuration

The addon uses the Mosquitto broker automatically via `mqtt:need` — no MQTT credentials are required unless you use an **external broker**.

| Option | Default | Description |
|---|---|---|
| `eebus_port` | `4714` | TCP port the EEBus service listens on. Must be reachable from the heat pump. |
| `MQTT_HOST` | _(Mosquitto)_ | Override MQTT broker hostname. Leave empty to use the built-in Mosquitto addon. |
| `MQTT_PORT` | `1883` | Override MQTT broker port. |
| `MQTT_USER` | _(empty)_ | MQTT username — only needed for an external broker. |
| `MQTT_PWD` | _(empty)_ | MQTT password — only needed for an external broker. |
| `discovery_prefix` | `homeassistant` | HA MQTT discovery prefix. Change only if you customised it in the MQTT integration. |
| `topic_prefix` | `eebus` | Prefix for all state and availability topics published by this addon. |

---

## Pairing a device

Pairing only needs to be done once. The certificate and trust are stored in `/data/` and survive addon updates and restarts.

1. **Start the addon.** On first run it generates a certificate and logs the **local SKI**:
   ```
   [CERT   ] NEW CERTIFICATE GENERATED
   [CERT   ] LOCAL SKI : 3a1f...
   [CERT   ] Add this SKI in the myVaillant app to enable pairing.
   ```
2. **Register the SKI** in your heat pump's app (e.g. myVaillant → Settings → Smart Home → EEBus → Add device). Enter the SKI shown in the logs.
3. The addon logs:
   ```
   [PAIR   ] pairing initiated — approve on device  SKI=24171f...
   [PAIR   ] waiting for user to approve pairing on device  SKI=24171f...
   ```
4. **Approve the pairing** in the app on the heat pump side.
5. Once accepted, the addon publishes the device and entities to MQTT:
   ```
   [MQTT   ] discovery published — 11 entities under "vr921_..."
   ```
   The device appears automatically in **Settings → Devices & Services → MQTT**.

---

## Topics

| Topic | Retained | Description |
|---|---|---|
| `eebus/<ski>/state` | ✅ | JSON payload with all current measurements |
| `eebus/<ski>/availability` | ✅ | `online` / `offline` |
| `homeassistant/device/eebus_<ski8>/config` | ✅ | MQTT discovery config (all entities) |

Example state payload:
```json
{
  "power": 1842.5
}
```

---

## Troubleshooting

**No devices discovered**  
→ Confirm the heat pump and HA host are on the same network. EEBus uses mDNS (UDP multicast port 5353) — check that multicast traffic is not blocked by a managed switch or VLAN.

**Pairing stuck at "waiting for user to approve"**  
→ Make sure you have added the addon's SKI in the heat pump app first. The heat pump initiates trust — the addon cannot force it.

**`remote feature not found` in logs**  
→ SPINE feature negotiation is still in progress. This resolves itself within a few seconds of connection. If it persists, the device may not support MA-MPC.

**Entities show as unavailable after HA restart**  
→ The addon re-publishes discovery when it receives HA's birth message on `homeassistant/status`. If this does not happen, restart the addon.

---

## Certificate & data persistence

The TLS certificate and private key are stored at:
```
/data/cert.pem
/data/key.pem
```
These files persist across addon updates. **Do not delete them** — the heat pump trusts this specific certificate. If you regenerate the certificate you will need to re-pair all devices.

// EEBus → Home Assistant MQTT bridge (MA-MPC use case).
//
// Boot flow:
//  1. Discover all EEBus devices on the network continuously via mDNS.
//  2. Initiate pairing with every discovered device automatically.
//  3. User approves the pairing on the device side (e.g. myVaillant app).
//  4. On successful pairing: publish HA MQTT device discovery (all sensors in
//     one message) and start forwarding measurement events.
//  5. On HA restart (birth message on homeassistant/status), re-publish
//     discovery and availability for all currently connected devices.
//
// Configuration — /data/options.json (HA addon) or environment variables:
//
// EEBUS_PORT            int    default 4714
// MQTT_HOST             string required
// MQTT_PORT             int    default 1883
// MQTT_USER             string optional
// MQTT_PASSWORD         string optional
// MQTT_DISCOVERY_PREFIX string default homeassistant
// MQTT_TOPIC_PREFIX     string default eebus
//
// Certificate is always stored at /data/cert.pem and /data/key.pem (not configurable).
package main

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/enbility/eebus-go/api"
	"github.com/enbility/eebus-go/features/client"
	"github.com/enbility/eebus-go/service"
	ucapi "github.com/enbility/eebus-go/usecases/api"
	"github.com/enbility/eebus-go/usecases/ma/mpc"
	shipapi "github.com/enbility/ship-go/api"
	"github.com/enbility/ship-go/cert"
	spineapi "github.com/enbility/spine-go/api"
	"github.com/enbility/spine-go/model"
)

// ── Configuration ─────────────────────────────────────────────────────────────

const (
	certFile = "/data/cert.pem"
	keyFile  = "/data/key.pem"
)

type Config struct {
	EEBusPort       int    `json:"eebus_port"`
	MQTTHost        string `json:"MQTT_HOST"`
	MQTTPort        int    `json:"MQTT_PORT"`
	MQTTUser        string `json:"MQTT_USER"`
	MQTTPassword    string `json:"MQTT_PWD"`
	DiscoveryPrefix string `json:"discovery_prefix"`
	TopicPrefix     string `json:"topic_prefix"`
}

func loadConfig() *Config {
	cfg := &Config{
		EEBusPort:       4714,
		MQTTHost:        "localhost",
		MQTTPort:        1883,
		DiscoveryPrefix: "homeassistant",
		TopicPrefix:     "eebus",
	}
	envInt := func(key string, dst *int) {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	envStr := func(key string, dst *string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	envInt("EEBUS_PORT", &cfg.EEBusPort)
	envStr("MQTT_HOST", &cfg.MQTTHost)
	envInt("MQTT_PORT", &cfg.MQTTPort)
	envStr("MQTT_USER", &cfg.MQTTUser)
	envStr("MQTT_PWD", &cfg.MQTTPassword)
	envStr("MQTT_DISCOVERY_PREFIX", &cfg.DiscoveryPrefix)
	envStr("MQTT_TOPIC_PREFIX", &cfg.TopicPrefix)
	return cfg
}

// ── Per-device measurement state ───────────────────────────────────────────────

type deviceState struct {
	mu sync.Mutex

	power    *float64
	powerL   []float64
	currentL []float64
	voltageL []float64
	freq     *float64
}

func (s *deviceState) setPower(w float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.power = &w
}

func (s *deviceState) payload() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := map[string]any{}
	if s.power != nil {
		p["power"] = *s.power
	}
	for i, v := range s.powerL {
		p[fmt.Sprintf("power_l%d", i+1)] = v
	}
	for i, v := range s.currentL {
		p[fmt.Sprintf("current_l%d", i+1)] = v
	}
	for i, v := range s.voltageL {
		p[fmt.Sprintf("voltage_l%d", i+1)] = v
	}
	if s.freq != nil {
		p["frequency"] = *s.freq
	}
	return p
}

// ── MQTT discovery structs ─────────────────────────────────────────────────────

// haDeviceDiscovery is the HA device discovery payload:
// https://www.home-assistant.io/integrations/mqtt/#device-discovery-payload
// A single message groups all sensor components under one HA device.
type haDeviceDiscovery struct {
	Device     haDevBlock             `json:"dev"`
	Origin     haOriginBlock          `json:"o"`
	StateTopic string                 `json:"state_topic"`
	AvailTopic string                 `json:"avty_t"`
	Components map[string]haComponent `json:"cmps"`
}

// haDevBlock maps to the abbreviated "dev" key in device discovery.
type haDevBlock struct {
	Identifiers  []string `json:"ids"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"mf,omitempty"`
	Model        string   `json:"mdl,omitempty"`
	SerialNumber string   `json:"sn,omitempty"`
	HWVersion    string   `json:"hw,omitempty"`
}

// haOriginBlock is required for device discovery; identifies the bridge.
type haOriginBlock struct {
	Name       string `json:"name"`
	SWVersion  string `json:"sw,omitempty"`
	SupportURL string `json:"url,omitempty"`
}

// haComponent is one sensor entry under the "cmps" key.
type haComponent struct {
	Platform          string `json:"p"`
	Name              string `json:"name"`
	UniqueID          string `json:"unique_id"`
	ValueTemplate     string `json:"value_template"`
	UnitOfMeasurement string `json:"unit_of_measurement,omitempty"`
	DeviceClass       string `json:"device_class,omitempty"`
	StateClass        string `json:"state_class,omitempty"`
	Icon              string `json:"icon,omitempty"`
	EnabledByDefault  *bool  `json:"enabled_by_default,omitempty"`
}

type sensorSpec struct {
	key            string
	name           string
	unit           string
	class          string
	sclass         string
	icon           string
	enabledDefault bool
}

var boolPtr = func(b bool) *bool { return &b }

// allSensors defines every sensor entity published to HA.
// Per-phase sensors are disabled by default — enable them in HA if your
// device sends per-phase data.
var allSensors = []sensorSpec{
	{"power", "Power", "W", "power", "measurement", "", true},
	{"power_l1", "Power L1", "W", "power", "measurement", "", false},
	{"power_l2", "Power L2", "W", "power", "measurement", "", false},
	{"power_l3", "Power L3", "W", "power", "measurement", "", false},
	{"current_l1", "Current L1", "A", "current", "measurement", "", false},
	{"current_l2", "Current L2", "A", "current", "measurement", "", false},
	{"current_l3", "Current L3", "A", "current", "measurement", "", false},
	{"voltage_l1", "Voltage L1", "V", "voltage", "measurement", "", false},
	{"voltage_l2", "Voltage L2", "V", "voltage", "measurement", "", false},
	{"voltage_l3", "Voltage L3", "V", "voltage", "measurement", "", false},
	{"frequency", "Frequency", "Hz", "frequency", "measurement", "", false},
}

// ── Bridge ─────────────────────────────────────────────────────────────────────

type bridge struct {
	cfg   *Config
	svc   *service.Service
	ucMpc ucapi.MaMPCInterface
	mqtt  mqtt.Client

	mu                sync.RWMutex
	mdnsInfo          map[string]shipapi.RemoteService          // SKI → mDNS info
	registeredSKIs    map[string]bool                           // SKIs we called RegisterRemoteSKI for
	discoveredSKIs    map[string]bool                           // SKIs with MQTT discovery published
	connectedEntities map[string]spineapi.EntityRemoteInterface // SKI → entity (for HA restart re-publish)
	states            map[string]*deviceState
}

func newBridge(cfg *Config) *bridge {
	return &bridge{
		cfg:               cfg,
		mdnsInfo:          make(map[string]shipapi.RemoteService),
		registeredSKIs:    make(map[string]bool),
		discoveredSKIs:    make(map[string]bool),
		connectedEntities: make(map[string]spineapi.EntityRemoteInterface),
		states:            make(map[string]*deviceState),
	}
}

func (b *bridge) getState(ski string) *deviceState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.states[ski]; ok {
		return s
	}
	s := &deviceState{}
	b.states[ski] = s
	return s
}

// ── MQTT helpers ───────────────────────────────────────────────────────────────

func (b *bridge) stateTopic(ski string) string {
	return fmt.Sprintf("%s/%s/state", b.cfg.TopicPrefix, ski)
}

func (b *bridge) availTopic(ski string) string {
	return fmt.Sprintf("%s/%s/availability", b.cfg.TopicPrefix, ski)
}

func (b *bridge) discoveryTopic(ski string) string {
	// Device discovery topic: <prefix>/device/<object_id>/config
	return fmt.Sprintf("%s/device/eebus_%s/config", b.cfg.DiscoveryPrefix, ski[:8])
}

func (b *bridge) mqttPublish(topic string, retain bool, payload any) {
	var data []byte
	switch v := payload.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		data, _ = json.Marshal(v)
	}
	b.mqtt.Publish(topic, 1, retain, data).Wait()
}

func (b *bridge) publishState(ski string) {
	b.mqttPublish(b.stateTopic(ski), true, b.getState(ski).payload())
}

// ── MQTT discovery ─────────────────────────────────────────────────────────────

// publishDiscovery publishes a single HA MQTT device discovery message that
// groups all sensor entities under one device.  Safe to call on every
// connection — HA treats re-publishes as config updates.
func (b *bridge) publishDiscovery(ski string, entity spineapi.EntityRemoteInterface) {
	b.mu.Lock()
	firstTime := !b.discoveredSKIs[ski]
	b.discoveredSKIs[ski] = true
	b.connectedEntities[ski] = entity
	info, hasInfo := b.mdnsInfo[ski]
	b.mu.Unlock()

	dev := haDevBlock{
		Identifiers: []string{"eebus_" + ski},
		Name:        "EEBus Device (" + ski[:8] + ")",
	}
	if hasInfo {
		if info.Name != "" {
			dev.Name = info.Name
		}
		dev.Manufacturer = info.Brand
		dev.Model = info.Model
		dev.SerialNumber = info.Identifier // e.g. "21223800202609620938070569N6"
		dev.HWVersion = string(info.Type)  // e.g. "Gateway"
	}

	idPrefix := "eebus_" + ski[:8]
	components := make(map[string]haComponent, len(allSensors))
	for _, spec := range allSensors {
		cmp := haComponent{
			Platform:          "sensor",
			Name:              spec.name,
			UniqueID:          idPrefix + "_" + spec.key,
			ValueTemplate:     fmt.Sprintf("{{ value_json.%s }}", spec.key),
			UnitOfMeasurement: spec.unit,
			DeviceClass:       spec.class,
			StateClass:        spec.sclass,
		}
		if spec.icon != "" {
			cmp.Icon = spec.icon
		}
		if !spec.enabledDefault {
			cmp.EnabledByDefault = boolPtr(false)
		}
		components[spec.key] = cmp
	}

	payload := haDeviceDiscovery{
		Device:     dev,
		Origin:     haOriginBlock{Name: "eebus-ha-bridge", SWVersion: "1.0.0"},
		StateTopic: b.stateTopic(ski),
		AvailTopic: b.availTopic(ski),
		Components: components,
	}

	b.mqttPublish(b.discoveryTopic(ski), true, payload)
	logf("MQTT", "discovery published — %d entities under %q  sn=%s  [SKI=%s]",
		len(allSensors), dev.Name, dev.SerialNumber, ski)

	if firstTime {
		b.logMeasurementDescriptions(ski, entity)
	}
}

// onBirthMessage is called when HA sends its birth message on homeassistant/status.
// We re-publish discovery and availability for every connected device so HA
// picks them up after a restart.
func (b *bridge) onBirthMessage(_ mqtt.Client, msg mqtt.Message) {
	if string(msg.Payload()) != "online" {
		return
	}
	b.mu.RLock()
	entities := make(map[string]spineapi.EntityRemoteInterface, len(b.connectedEntities))
	for k, v := range b.connectedEntities {
		entities[k] = v
	}
	b.mu.RUnlock()

	if len(entities) == 0 {
		return
	}
	logf("MQTT", "HA birth message — re-publishing discovery for %d device(s)", len(entities))
	for ski, entity := range entities {
		b.publishDiscovery(ski, entity)
		b.mqttPublish(b.availTopic(ski), true, "online")
	}
}

// ── MA-MPC event handler ───────────────────────────────────────────────────────

func (b *bridge) onMpcEvent(ski string, _ spineapi.DeviceRemoteInterface, entity spineapi.EntityRemoteInterface, event api.EventType) {
	s := b.getState(ski)

	switch event {
	case mpc.UseCaseSupportUpdate:
		b.publishDiscovery(ski, entity)
		b.mqttPublish(b.availTopic(ski), true, "online")
		return

	case mpc.DataUpdatePower:
		if v, err := b.ucMpc.Power(entity); err == nil {
			s.setPower(v)
			logf("MPC", "Power: %.2f W  [SKI=%s]", v, ski)
		}
	case mpc.DataUpdatePowerPerPhase:
		if v, err := b.ucMpc.PowerPerPhase(entity); err == nil {
			s.mu.Lock()
			s.powerL = v
			s.mu.Unlock()
			logf("MPC", "PowerPerPhase: L1=%.2f W  L2=%.2f W  L3=%.2f W  [SKI=%s]", val(v, 0), val(v, 1), val(v, 2), ski)
		}
	case mpc.DataUpdateCurrentsPerPhase:
		if v, err := b.ucMpc.CurrentPerPhase(entity); err == nil {
			s.mu.Lock()
			s.currentL = v
			s.mu.Unlock()
			logf("MPC", "CurrentPerPhase: L1=%.2f A  L2=%.2f A  L3=%.2f A  [SKI=%s]", val(v, 0), val(v, 1), val(v, 2), ski)
		}
	case mpc.DataUpdateVoltagePerPhase:
		if v, err := b.ucMpc.VoltagePerPhase(entity); err == nil {
			s.mu.Lock()
			s.voltageL = v
			s.mu.Unlock()
			logf("MPC", "VoltagePerPhase: L1=%.2f V  L2=%.2f V  L3=%.2f V  [SKI=%s]", val(v, 0), val(v, 1), val(v, 2), ski)
		}
	case mpc.DataUpdateFrequency:
		if v, err := b.ucMpc.Frequency(entity); err == nil {
			s.mu.Lock()
			s.freq = &v
			s.mu.Unlock()
			logf("MPC", "Frequency: %.2f Hz  [SKI=%s]", v, ski)
		}
	default:
		logf("MPC", "unknown event: %v  [SKI=%s]", event, ski)
		return
	}

	b.publishState(ski)
}

// ── SHIP / pairing callbacks ───────────────────────────────────────────────────

func (b *bridge) RemoteSKIConnected(_ api.ServiceInterface, ski string) {
	logf("SHIP", "connected  SKI=%s", ski)
}

func (b *bridge) RemoteSKIDisconnected(_ api.ServiceInterface, ski string) {
	logf("SHIP", "disconnected  SKI=%s", ski)
	b.mqttPublish(b.availTopic(ski), true, "offline")

	// Clear so discovery re-publishes on next connect
	b.mu.Lock()
	delete(b.discoveredSKIs, ski)
	delete(b.connectedEntities, ski)
	b.mu.Unlock()
}

// VisibleRemoteServicesUpdated fires whenever mDNS sees a change.
// We register every new SKI for pairing automatically.
func (b *bridge) VisibleRemoteServicesUpdated(_ api.ServiceInterface, entries []shipapi.RemoteService) {
	b.mu.Lock()
	b.mdnsInfo = make(map[string]shipapi.RemoteService, len(entries))
	for _, e := range entries {
		b.mdnsInfo[e.Ski] = e
	}
	b.mu.Unlock()

	logf("DISC", "%d device(s) visible on network", len(entries))

	for _, e := range entries {
		b.mu.Lock()
		already := b.registeredSKIs[e.Ski]
		if !already {
			b.registeredSKIs[e.Ski] = true
		}
		b.mu.Unlock()

		if !already {
			b.svc.RegisterRemoteSKI(e.Ski)
			logf("PAIR", "pairing initiated — approve on device  SKI=%s  name=%s  model=%s",
				e.Ski, e.Name, e.Model)
		}
	}
}

func (b *bridge) ServiceShipIDUpdate(ski, shipID string) {
	logf("SHIP", "ship-id  SKI=%s  id=%s", ski, shipID)
}

// ServicePairingDetailUpdate logs pairing progress and handles rejection.
func (b *bridge) ServicePairingDetailUpdate(ski string, detail *shipapi.ConnectionStateDetail) {
	state := detail.State()
	logf("PAIR", "SKI=%s  state=%s", ski, connectionStateName(state))

	switch state {
	case shipapi.ConnectionStateRemoteDeniedTrust, shipapi.ConnectionStateError:
		logf("PAIR", "pairing rejected by device  SKI=%s", ski)
		b.svc.CancelPairingWithSKI(ski)
		// Unregister so we try again on next discovery cycle
		b.mu.Lock()
		delete(b.registeredSKIs, ski)
		b.mu.Unlock()
	}
}

func connectionStateName(s shipapi.ConnectionState) string {
	names := map[shipapi.ConnectionState]string{
		shipapi.ConnectionStateNone:                "none",
		shipapi.ConnectionStateQueued:              "queued",
		shipapi.ConnectionStateInitiated:           "initiated",
		shipapi.ConnectionStateReceivedPairingRequest: "pairing-requested",
		shipapi.ConnectionStateInProgress:          "in-progress",
		shipapi.ConnectionStateTrusted:             "trusted",
		shipapi.ConnectionStatePin:                 "pin",
		shipapi.ConnectionStateCompleted:           "completed",
		shipapi.ConnectionStateRemoteDeniedTrust:   "remote-denied",
		shipapi.ConnectionStateError:               "error",
	}
	if n, ok := names[s]; ok {
		return n
	}
	return fmt.Sprintf("unknown(%d)", s)
}

// AllowWaitingForTrust returns true so we wait for the user to approve
// the pairing on the device side (e.g. the myVaillant app).
func (b *bridge) AllowWaitingForTrust(ski string) bool {
	logf("PAIR", "waiting for user to approve pairing on device  SKI=%s", ski)
	return true
}

// ── Setup & main ───────────────────────────────────────────────────────────────

func (b *bridge) setupMQTT() {
	url := fmt.Sprintf("tcp://%s:%d", b.cfg.MQTTHost, b.cfg.MQTTPort)
	opts := mqtt.NewClientOptions().
		AddBroker(url).
		SetClientID("eebus-mpc-bridge").
		SetKeepAlive(30 * time.Second).
		SetAutoReconnect(true).
		SetOnConnectHandler(func(c mqtt.Client) {
			logf("MQTT", "connected to broker %s", url)
			// Subscribe to HA birth/death messages so we can re-publish
			// discovery when HA restarts (before we receive any new events).
			c.Subscribe(b.cfg.DiscoveryPrefix+"/status", 1, b.onBirthMessage)
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			logf("MQTT", "broker connection lost: %v", err)
		})
	if b.cfg.MQTTUser != "" {
		opts.SetUsername(b.cfg.MQTTUser).SetPassword(b.cfg.MQTTPassword)
	}
	b.mqtt = mqtt.NewClient(opts)
	for {
		if token := b.mqtt.Connect(); token.Wait() && token.Error() != nil {
			logf("MQTT", "connect failed: %v — retrying in 5s", token.Error())
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}
}

// hostSerialNumber was removed — using fixed serial "HA-BRIDGE-001".

func (b *bridge) setupEEBus(certificate tls.Certificate) {
	cfg, err := api.NewConfiguration(
		"HomeAssistant",  // vendorCode  (IANA PEN / vendor identifier)
		"Home Assistant", // deviceBrand (shown in mDNS + SPINE)
		"EEBus Bridge",   // deviceModel
		"HA-BRIDGE-001",  // serialNumber
		model.DeviceTypeTypeEnergyManagementSystem,
		[]model.EntityTypeType{model.EntityTypeTypeCEM},
		b.cfg.EEBusPort,
		certificate,
		4*time.Second,
	)
	if err != nil {
		log.Fatal(err)
	}
	b.svc = service.NewService(cfg, b)
	b.svc.SetLogging(b)
	if err := b.svc.Setup(); err != nil {
		log.Fatal(err)
	}
	localEntity := b.svc.LocalDevice().EntityForType(model.EntityTypeTypeCEM)
	b.ucMpc = mpc.NewMPC(localEntity, b.onMpcEvent)
	b.svc.AddUseCase(b.ucMpc)
}

func main() {
	cfg := loadConfig()
	certificate := loadOrCreateCert(cfg)

	b := newBridge(cfg)
	b.setupMQTT()
	b.setupEEBus(certificate)
	b.svc.Start()

	logf("INFO", "EEBus bridge running on port %d — scanning for devices…", cfg.EEBusPort)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	logf("INFO", "shutting down")
	b.svc.Shutdown()
	b.mqtt.Disconnect(500)
}

// ── Certificate ────────────────────────────────────────────────────────────────

func loadOrCreateCert(cfg *Config) tls.Certificate {
	if _, err := os.Stat(certFile); err == nil {
		if c, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
			logf("CERT", "loaded from %s", certFile)
			return c
		}
	}
	c, err := cert.CreateCertificate("EEBus Bridge", "Home Assistant", "BE", "HA-BRIDGE-001")
	if err != nil {
		log.Fatal(err)
	}
	certOut, _ := os.Create(certFile)
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]})
	certOut.Close()
	keyOut, _ := os.Create(keyFile)
	b, _ := x509.MarshalECPrivateKey(c.PrivateKey.(*ecdsa.PrivateKey))
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
	keyOut.Close()
	x509Cert, _ := x509.ParseCertificate(c.Certificate[0])
	if ski, err := cert.SkiFromCertificate(x509Cert); err == nil {
		logf("CERT", "════════════════════════════════════════════════════════")
		logf("CERT", " NEW CERTIFICATE GENERATED")
		logf("CERT", " LOCAL SKI : %s", ski)
		logf("CERT", " Add this SKI in the myVaillant app to enable pairing.")
		logf("CERT", "════════════════════════════════════════════════════════")
	}
	logf("CERT", "saved to %s / %s", certFile, keyFile)
	return c
}

// ── Diagnostics ────────────────────────────────────────────────────────────────

func (b *bridge) logMeasurementDescriptions(ski string, entity spineapi.EntityRemoteInterface) {
	localEntity := b.svc.LocalDevice().EntityForType(model.EntityTypeTypeCEM)
	m, err := client.NewMeasurement(localEntity, entity)
	if err != nil {
		return
	}
	descs, err := m.GetDescriptionsForFilter(model.MeasurementDescriptionDataType{})
	if err != nil || len(descs) == 0 {
		return
	}
	logf("MEAS", "SKI=%s  ── advertised measurements (%d) ──", ski, len(descs))
	for _, d := range descs {
		logf("MEAS", "  id=%-3s type=%-12s scope=%-22s unit=%s",
			fmtMeasID(d.MeasurementId), strVal(d.MeasurementType),
			strVal(d.ScopeType), strVal(d.Unit))
	}
}

func fmtMeasID(id *model.MeasurementIdType) string {
	if id == nil {
		return "?"
	}
	return strconv.Itoa(int(*id))
}

func strVal[T ~string](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return string(*p)
}

// ── Logging (ship-go logging.Logging interface) ────────────────────────────────

func (b *bridge) Trace(args ...interface{})            {}
func (b *bridge) Tracef(f string, _ ...interface{})    {}
func (b *bridge) Debug(args ...interface{})            {}
func (b *bridge) Debugf(f string, _ ...interface{})    {}
func (b *bridge) Info(args ...interface{})             { logf("INFO", fmt.Sprint(args...)) }
func (b *bridge) Infof(f string, args ...interface{})  { logf("INFO", fmt.Sprintf(f, args...)) }
func (b *bridge) Error(args ...interface{})            { logf("ERROR", fmt.Sprint(args...)) }
func (b *bridge) Errorf(f string, args ...interface{}) { logf("ERROR", fmt.Sprintf(f, args...)) }

func logf(tag, format string, args ...interface{}) {
	fmt.Printf("%s [%-7s] %s\n", time.Now().Format("15:04:05.000"), tag, fmt.Sprintf(format, args...))
}

// val safely indexes a float64 slice, returning 0 if out of bounds.
func val(s []float64, i int) float64 {
	if i < len(s) {
		return s[i]
	}
	return 0
}

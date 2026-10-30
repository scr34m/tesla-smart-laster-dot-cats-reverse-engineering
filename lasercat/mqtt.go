package main

// Home Assistant MQTT bridge: keeps the BLE link up, reconnects when it drops,
// and announces via MQTT discovery
//   - a light: on/off, effects on/small/medium/large/manual, and the
//     colour wheel as laser position (hue = direction, saturation = distance)
//   - a text status sensor.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"tinygo.org/x/bluetooth"
)

const keepAliveInterval = time.Minute

const effectManual = "manual"

var effects = append(slices.Clone(modeNames), effectManual)

type dpWrite struct {
	id, typ byte
	value   []byte
}

// command is a sequence of DP writes plus the state change it causes.
type command struct {
	writes []dpWrite
	apply  func(*lightState)
}

type lightState struct {
	On     bool
	Effect string
	H, S   float64
}

type bridge struct {
	cfg       config
	mq        mqtt.Client
	cmds      chan command
	connected atomic.Bool

	mu      sync.Mutex
	light   lightState
	autoOff *time.Timer
}

func (b *bridge) t(suffix string) string { return b.cfg.topic + "/" + suffix }

func (b *bridge) publish(topic, payload string) {
	b.mq.Publish(topic, 0, true, payload)
}

// setStatus publishes the human readable bridge state (text sensor).
func (b *bridge) setStatus(text string) {
	if len(text) > 250 {
		text = text[:250]
	}
	log.Println("status:", text)
	b.publish(b.t("status"), text)
}

func (b *bridge) setAvailable(online bool) {
	b.connected.Store(online)
	b.publish(b.t("availability"), map[bool]string{true: "online", false: "offline"}[online])
}

// update changes the light state and publishes it. The firmware stops by
// itself after 5 minutes without reporting it, so switching on starts a timer
// that shows the light off again; only commands (which reset the firmware's
// counter) or an off→on change restart it, repeated "on" reports do not.
func (b *bridge) update(change func(*lightState), fromCommand bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	wasOn := b.light.On
	change(&b.light)
	switch {
	case !b.light.On:
		b.autoOff.Stop()
	case (fromCommand || !wasOn) && b.cfg.autoOff > 0:
		b.autoOff.Reset(b.cfg.autoOff)
	}

	state := map[string]any{
		"state":      map[bool]string{true: "ON", false: "OFF"}[b.light.On],
		"color_mode": "hs",
		"color":      map[string]float64{"h": b.light.H, "s": b.light.S},
	}
	if b.light.Effect != "" {
		state["effect"] = b.light.Effect
	}
	payload, _ := json.Marshal(state)
	b.publish(b.t("light/state"), string(payload))
}

func (b *bridge) discovery() {
	id := "lasercat_" + b.cfg.deviceID
	device := map[string]any{
		"identifiers":  []string{id},
		"name":         "Laser Cat",
		"manufacturer": "Tesla Smart",
		"model":        "Laser Dot Cats (gcslbvj0)",
	}
	configs := map[string]map[string]any{
		"light/laser": {
			"name":                  "Laser",
			"schema":                "json",
			"command_topic":         b.t("light/set"),
			"state_topic":           b.t("light/state"),
			"supported_color_modes": []string{"hs"},
			"brightness":            false,
			"effect":                true,
			"effect_list":           effects,
			"availability":          []map[string]string{{"topic": b.t("availability")}},
			"icon":                  "mdi:laser-pointer",
		},
		"sensor/status": {
			"name":        "Status",
			"state_topic": b.t("status"),
			"icon":        "mdi:bluetooth-connect",
		},
	}
	for path, c := range configs {
		component, object, _ := strings.Cut(path, "/")
		c["unique_id"] = id + "_" + object
		c["object_id"] = "lasercat_" + object
		c["device"] = device
		payload, _ := json.Marshal(c)
		b.publish(fmt.Sprintf("homeassistant/%s/%s/%s/config", component, id, object), string(payload))
	}
	// Remove the entities of the previous switch/select version.
	b.publish(fmt.Sprintf("homeassistant/switch/%s/power/config", id), "")
	b.publish(fmt.Sprintf("homeassistant/select/%s/mode/config", id), "")
}

// moveWrite converts a colour-wheel position to the DP 102 X/Y payload.
func (b *bridge) moveWrite(h, s float64) dpWrite {
	r := math.Max(0, math.Min(s, 100)) / 100 * b.cfg.xyRadius
	a := h * math.Pi / 180
	coord := func(v float64) uint16 {
		return uint16(math.Round(math.Max(0, math.Min(2*xyCentre, xyCentre+v))))
	}
	value := binary.BigEndian.AppendUint16(nil, coord(r*math.Cos(a)))
	value = binary.BigEndian.AppendUint16(value, coord(-r*math.Sin(a))) // Y inverted: up on the wheel = up in the room
	return dpWrite{dpMove, dpTypeRaw, value}
}

func switchWrite(on bool) dpWrite {
	v := byte(0)
	if on {
		v = 1
	}
	return dpWrite{dpSwitch, dpTypeBool, []byte{v}}
}

// buildCommand turns a Home Assistant JSON light command into DP writes.
func (b *bridge) buildCommand(payload []byte) (command, error) {
	var req struct {
		State  string
		Effect *string
		Color  *struct{ H, S float64 }
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return command{}, err
	}
	b.mu.Lock()
	cur := b.light
	b.mu.Unlock()

	if strings.EqualFold(req.State, "OFF") {
		return command{[]dpWrite{switchWrite(false)}, func(l *lightState) { l.On = false }}, nil
	}

	move := func(h, s float64) command {
		var w []dpWrite
		// The firmware ignores X/Y while the arc pattern ("on") runs; switching
		// off first clears that lock without moving the laser around.
		if cur.Effect == modeOn {
			w = append(w, switchWrite(false))
		}
		w = append(w, b.moveWrite(h, s))
		return command{w, func(l *lightState) { l.On, l.Effect, l.H, l.S = true, effectManual, h, s }}
	}

	switch {
	case req.Color != nil:
		return move(req.Color.H, req.Color.S), nil
	case req.Effect != nil:
		e := *req.Effect
		if e == effectManual {
			return move(cur.H, cur.S), nil
		}
		if !slices.Contains(modeNames, e) {
			return command{}, fmt.Errorf("unknown effect %q", e)
		}
		id, typ, value, err := dpForCommand(e)
		if err != nil {
			return command{}, err
		}
		return command{[]dpWrite{{id, typ, value}}, func(l *lightState) { l.On, l.Effect = true, e }}, nil
	case cur.On:
		return command{nil, func(*lightState) {}}, nil // already on, just republish
	default:
		return command{[]dpWrite{switchWrite(true)}, func(l *lightState) { l.On, l.Effect = true, modeOn }}, nil
	}
}

func (b *bridge) onCommand(_ mqtt.Client, msg mqtt.Message) {
	cmd, err := b.buildCommand(msg.Payload())
	if err != nil {
		log.Printf("ignoring %s: %v", msg.Payload(), err)
		return
	}
	if !b.connected.Load() {
		b.setStatus("not connected, command ignored")
		return
	}
	select {
	case b.cmds <- cmd:
	default:
		log.Println("command queue full, dropping command")
	}
}

func (b *bridge) onDP(id, typ byte, v []byte) {
	switch {
	case id == dpSwitch && typ == dpTypeBool && len(v) == 1:
		b.update(func(l *lightState) { l.On = v[0] != 0 }, false)
	case id == dpMode && typ == dpTypeValue && len(v) == 4:
		switch m := binary.BigEndian.Uint32(v); {
		case m == 0:
			// The patched firmware (stub at 0x7000) reports mode 0 when its
			// play timer stops the laser.
			b.update(func(l *lightState) { l.On = false }, false)
		case int(m) < len(modeNames): // m >= 1 here
			b.update(func(l *lightState) { l.Effect = modeNames[m] }, false)
		}
	}
}

func runBridge(cfg config) error {
	b := &bridge{cfg: cfg, cmds: make(chan command, 8)}
	b.autoOff = time.AfterFunc(time.Hour, func() { b.update(func(l *lightState) { l.On = false }, false) })
	b.autoOff.Stop()

	opts := mqtt.NewClientOptions().
		AddBroker(cfg.broker).
		SetClientID("lasercat-"+cfg.deviceID).
		SetUsername(cfg.mqttUser).
		SetPassword(cfg.mqttPass).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetMaxReconnectInterval(time.Minute).
		SetWill(b.t("availability"), "offline", 0, true).
		SetOnConnectHandler(func(c mqtt.Client) {
			log.Println("MQTT connected to", cfg.broker)
			b.discovery()
			b.setAvailable(b.connected.Load())
			b.update(func(*lightState) {}, false)
			c.Subscribe(b.t("light/set"), 0, b.onCommand)
			// Re-announce when Home Assistant restarts.
			c.Subscribe("homeassistant/status", 0, func(_ mqtt.Client, m mqtt.Message) {
				if string(m.Payload()) == "online" {
					b.discovery()
				}
			})
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			log.Println("MQTT connection lost:", err)
		})
	b.mq = mqtt.NewClient(opts)
	b.mq.Connect() // retries in the background

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		b.setStatus("stopped")
		b.mq.Publish(b.t("availability"), 0, true, "offline").WaitTimeout(2 * time.Second)
		b.mq.Disconnect(500)
		os.Exit(0)
	}()

	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return err
	}
	disconnected := make(chan struct{}, 1)
	adapter.SetConnectionStateHandler(func(_ bluetooth.Device, st bluetooth.ConnectionState) {
		if !st.Connected {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}
	})

	backoff := 5 * time.Second
	for {
		select { // drop a stale disconnect event from the previous link
		case <-disconnected:
		default:
		}
		dev, s, err := connect(adapter, cfg, b.setStatus)
		if err != nil {
			b.setStatus(fmt.Sprintf("error: %v (retry in %s)", err, backoff))
			time.Sleep(backoff)
			backoff = min(backoff*2, 2*time.Minute)
			continue
		}
		backoff = 5 * time.Second
		s.onDP = b.onDP
		b.setStatus("connected")
		b.setAvailable(true)

		reason := b.serve(s, disconnected)

		b.setAvailable(false)
		dev.Disconnect()
		b.setStatus("disconnected: " + reason)
		time.Sleep(2 * time.Second)
	}
}

// serve runs one BLE session until the link is lost; it returns the reason.
func (b *bridge) serve(s *session, disconnected <-chan struct{}) string {
	if err := s.status(); err != nil { // initial state
		return err.Error()
	}
	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case f := <-s.frames:
			s.handle(f)
		case c := <-b.cmds:
			for _, w := range c.writes {
				if err := s.setDP(w.id, w.typ, w.value); err != nil {
					return err.Error()
				}
			}
			b.update(c.apply, true)
		case <-ticker.C:
			if err := s.status(); err != nil {
				return "keep-alive failed: " + err.Error()
			}
		case <-disconnected:
			return "link lost"
		}
	}
}

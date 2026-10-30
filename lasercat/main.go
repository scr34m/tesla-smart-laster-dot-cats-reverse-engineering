// lasercat controls the Tesla Smart laser cat toy directly over Bluetooth LE,
// without the Tuya app or gateway.
//
//	lasercat [flags] on|off|small|medium|large   one-shot command
//	lasercat [flags] mqtt                        Home Assistant MQTT bridge
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"tinygo.org/x/bluetooth"
)

const tuyaCompanyID = 0x07D0

var (
	notifyUUID = bluetooth.New16BitUUID(0x2B10)
	writeUUID  = bluetooth.New16BitUUID(0x2B11)
)

// DPs of this device (see ../docs/tuya-protocol.md).
const (
	dpMove   = 102 // raw: X, Y as big-endian u16, xyCentre = calibrated centre
	dpMode   = 104 // value: 1 small, 2 medium, 3 large
	dpSwitch = 105 // bool: on / off

	xyCentre = 200
)

const modeOn = "on" // built-in arc pattern

// Index = DP 104 value. The built-in arc pattern (index 0) is started with
// DP 105 = true instead: DP 104 = 0 only holds the laser in place, and the
// patched firmware reports it as its "play timer finished" marker.
var modeNames = []string{modeOn, "small", "medium", "large"}

type config struct {
	addr, uuid, localKey, deviceID string
	scanTimeout                    time.Duration

	broker, mqttUser, mqttPass, topic string
	autoOff                           time.Duration
	xyRadius                          float64
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	var cfg config
	flag.StringVar(&cfg.addr, "addr", env("LASERCAT_ADDR", ""), "CoreBluetooth UUID of the device (default: first Tuya device found)")
	flag.StringVar(&cfg.uuid, "uuid", env("LASERCAT_UUID", ""), "Tuya device uuid")
	flag.StringVar(&cfg.localKey, "key", env("LASERCAT_KEY", ""), "Tuya local key")
	flag.StringVar(&cfg.deviceID, "devid", env("LASERCAT_DEVID", ""), "Tuya device id")
	flag.DurationVar(&cfg.scanTimeout, "timeout", 15*time.Second, "scan timeout")
	flag.StringVar(&cfg.broker, "broker", env("LASERCAT_BROKER", "tcp://192.168.1.21:1883"), "MQTT broker (mqtt mode)")
	flag.StringVar(&cfg.mqttUser, "mqtt-user", env("LASERCAT_MQTT_USER", ""), "MQTT user name")
	flag.StringVar(&cfg.mqttPass, "mqtt-pass", env("LASERCAT_MQTT_PASS", ""), "MQTT password")
	flag.StringVar(&cfg.topic, "topic", env("LASERCAT_TOPIC", "lasercat"), "MQTT base topic")
	flag.DurationVar(&cfg.autoOff, "auto-off", 5*time.Minute, "firmware play time after which the light is shown off (0 = never)")
	flag.Float64Var(&cfg.xyRadius, "xy-radius", 200, "laser offset at 100% colour saturation (DP 102 units, 200 = full range)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: lasercat [flags] on|off|small|medium|large|mqtt")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 || cfg.uuid == "" || len(cfg.localKey) < 6 || cfg.deviceID == "" {
		flag.Usage()
		os.Exit(2)
	}
	cmd := strings.ToLower(flag.Arg(0))
	var err error
	if cmd == "mqtt" {
		err = runBridge(cfg)
	} else {
		err = runOnce(cmd, cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// dpForCommand maps a command word to the DP write it needs.
func dpForCommand(cmd string) (id, typ byte, value []byte, err error) {
	switch cmd {
	case modeOn:
		return dpSwitch, dpTypeBool, []byte{1}, nil
	case "off":
		return dpSwitch, dpTypeBool, []byte{0}, nil
	}
	for v, name := range modeNames[1:] {
		if name == cmd {
			return dpMode, dpTypeValue, binary.BigEndian.AppendUint32(nil, uint32(v+1)), nil
		}
	}
	return 0, 0, nil, fmt.Errorf("unknown command %q", cmd)
}

func runOnce(cmd string, cfg config) error {
	id, typ, value, err := dpForCommand(cmd)
	if err != nil {
		return err
	}
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return err
	}
	dev, s, err := connect(adapter, cfg, func(msg string) { fmt.Println(msg) })
	if err != nil {
		return err
	}
	defer dev.Disconnect()
	s.onDP = printDP

	fmt.Println("Sending", cmd)
	if err := s.setDP(id, typ, value); err != nil {
		return err
	}
	// Print the state the device reports back.
	end := time.After(2 * time.Second)
	for {
		select {
		case f := <-s.frames:
			s.handle(f)
		case <-end:
			return nil
		}
	}
}

// connect scans for the device, connects, enables notifications and pairs.
func connect(adapter *bluetooth.Adapter, cfg config, progress func(string)) (bluetooth.Device, *session, error) {
	progress("scanning")
	found, proto, err := scan(adapter, cfg.addr, cfg.scanTimeout)
	if err != nil {
		return bluetooth.Device{}, nil, err
	}
	progress("connecting to " + found.String())
	dev, err := adapter.Connect(found, bluetooth.ConnectionParams{})
	if err != nil {
		return dev, nil, err
	}
	fail := func(err error) (bluetooth.Device, *session, error) {
		dev.Disconnect()
		return dev, nil, err
	}

	notify, write, err := characteristics(dev)
	if err != nil {
		return fail(err)
	}
	s := newSession(cfg.localKey, proto, func(p []byte) error {
		_, err := write.WriteWithoutResponse(p)
		return err
	})
	if err := notify.EnableNotifications(s.onNotify); err != nil {
		return fail(err)
	}
	progress("pairing")
	if err := s.login(cfg.uuid, cfg.deviceID); err != nil {
		return fail(err)
	}
	return dev, s, nil
}

// scan returns the device address and its Tuya BLE protocol version.
func scan(adapter *bluetooth.Adapter, want string, timeout time.Duration) (bluetooth.Address, byte, error) {
	var found bluetooth.Address
	proto := byte(3)
	ok := false
	timer := time.AfterFunc(timeout, func() { adapter.StopScan() })
	defer timer.Stop()

	err := adapter.Scan(func(a *bluetooth.Adapter, r bluetooth.ScanResult) {
		if want != "" && !strings.EqualFold(r.Address.String(), want) {
			return
		}
		for _, m := range r.ManufacturerData() {
			if m.CompanyID == tuyaCompanyID && len(m.Data) > 6 {
				found, proto, ok = r.Address, m.Data[1], true
				a.StopScan()
				return
			}
		}
	})
	if err != nil {
		return found, 0, err
	}
	if !ok {
		return found, 0, fmt.Errorf("device not found within %s", timeout)
	}
	return found, proto, nil
}

func characteristics(dev bluetooth.Device) (notify, write bluetooth.DeviceCharacteristic, err error) {
	services, err := dev.DiscoverServices(nil)
	if err != nil {
		return
	}
	var haveNotify, haveWrite bool
	for _, svc := range services {
		chars, err := svc.DiscoverCharacteristics([]bluetooth.UUID{notifyUUID, writeUUID})
		if err != nil {
			continue
		}
		for _, c := range chars {
			switch c.UUID() {
			case notifyUUID:
				notify, haveNotify = c, true
			case writeUUID:
				write, haveWrite = c, true
			}
		}
	}
	if !haveNotify || !haveWrite {
		err = fmt.Errorf("Tuya characteristics 2b10/2b11 not found")
	}
	return
}

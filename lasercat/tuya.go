package main

// Minimal Tuya BLE protocol (v3): key exchange, pairing and DP writes.
// Ported from ../ble_tuya_client/tuya_ble/tuya_ble.py.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const (
	gattMTU = 20

	codeDeviceInfo = 0x0000
	codePair       = 0x0001
	codeDPs        = 0x0002
	codeStatus     = 0x0003 // device answers and re-reports all DPs

	codeReceiveDP         = 0x8001
	codeReceiveTimeDP     = 0x8003
	codeReceiveSignDP     = 0x8004
	codeReceiveSignTimeDP = 0x8005
	codeTime1Req          = 0x8011
	codeTime2Req          = 0x8012

	secLogin   = 4 // AES key: md5(local_key[:6])
	secSession = 5 // AES key: md5(local_key[:6] + srand)

	dpTypeRaw   = 0
	dpTypeBool  = 1
	dpTypeValue = 2
)

type frame struct {
	seq, responseTo uint32
	code            uint16
	data            []byte
}

type session struct {
	write    func([]byte) error
	proto    byte
	localKey []byte
	loginKey []byte
	sessKey  []byte
	seq      uint32

	rx       []byte
	rxNext   uint32
	rxLength uint32
	frames   chan frame

	onDP func(id, typ byte, value []byte) // called for every DP the device reports
}

func newSession(localKey string, proto byte, write func([]byte) error) *session {
	lk := []byte(localKey)[:6]
	sum := md5.Sum(lk)
	return &session{
		write:    write,
		proto:    proto,
		localKey: lk,
		loginKey: sum[:],
		seq:      1,
		frames:   make(chan frame, 16),
	}
}

func crc16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

func appendVarint(b []byte, v uint32) []byte {
	for {
		c := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		b = append(b, c)
		if v == 0 {
			return b
		}
	}
}

func readVarint(b []byte, pos int) (uint32, int, error) {
	var v uint32
	for i := 0; i < 5; i++ {
		if pos+i >= len(b) {
			return 0, 0, errors.New("truncated varint")
		}
		v |= uint32(b[pos+i]&0x7F) << (7 * i)
		if b[pos+i]&0x80 == 0 {
			return v, pos + i + 1, nil
		}
	}
	return 0, 0, errors.New("varint too long")
}

func (s *session) key(flag byte) []byte {
	if flag == secLogin {
		return s.loginKey
	}
	return s.sessKey
}

// send encrypts one command and writes it in 20-byte GATT chunks.
func (s *session) send(code uint16, data []byte, responseTo uint32) (uint32, error) {
	seq := s.seq
	s.seq++

	raw := binary.BigEndian.AppendUint32(nil, seq)
	raw = binary.BigEndian.AppendUint32(raw, responseTo)
	raw = binary.BigEndian.AppendUint16(raw, code)
	raw = binary.BigEndian.AppendUint16(raw, uint16(len(data)))
	raw = append(raw, data...)
	raw = binary.BigEndian.AppendUint16(raw, crc16(raw))
	for len(raw)%16 != 0 {
		raw = append(raw, 0)
	}

	flag := byte(secSession)
	if code == codeDeviceInfo {
		flag = secLogin
	}
	block, err := aes.NewCipher(s.key(flag))
	if err != nil {
		return 0, err
	}
	iv := make([]byte, 16)
	if _, err := rand.Read(iv); err != nil {
		return 0, err
	}
	enc := append([]byte{flag}, iv...)
	ct := make([]byte, len(raw))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, raw)
	enc = append(enc, ct...)

	for num, pos := uint32(0), 0; pos < len(enc); num++ {
		pkt := appendVarint(nil, num)
		if num == 0 {
			pkt = appendVarint(pkt, uint32(len(enc)))
			pkt = append(pkt, s.proto<<4)
		}
		n := min(gattMTU-len(pkt), len(enc)-pos)
		pkt = append(pkt, enc[pos:pos+n]...)
		pos += n
		if err := s.write(pkt); err != nil {
			return 0, err
		}
	}
	return seq, nil
}

// onNotify reassembles notification chunks and queues decrypted frames.
func (s *session) onNotify(chunk []byte) {
	num, pos, err := readVarint(chunk, 0)
	if err != nil {
		return
	}
	if num == 0 {
		s.rx = s.rx[:0]
		s.rxNext = 0
		if s.rxLength, pos, err = readVarint(chunk, pos); err != nil {
			return
		}
		pos++ // protocol version byte
	}
	if num != s.rxNext {
		s.rxNext = 0
		return
	}
	s.rx = append(s.rx, chunk[pos:]...)
	s.rxNext++
	if uint32(len(s.rx)) < s.rxLength {
		return
	}
	s.rxNext = 0
	if f, err := s.decrypt(s.rx); err == nil {
		s.frames <- f
	}
}

func (s *session) decrypt(buf []byte) (frame, error) {
	if len(buf) < 33 || (len(buf)-17)%16 != 0 {
		return frame{}, errors.New("bad length")
	}
	key := s.key(buf[0])
	if key == nil {
		return frame{}, errors.New("no key yet")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return frame{}, err
	}
	raw := make([]byte, len(buf)-17)
	cipher.NewCBCDecrypter(block, buf[1:17]).CryptBlocks(raw, buf[17:])
	f := frame{
		seq:        binary.BigEndian.Uint32(raw[0:]),
		responseTo: binary.BigEndian.Uint32(raw[4:]),
		code:       binary.BigEndian.Uint16(raw[8:]),
	}
	n := int(binary.BigEndian.Uint16(raw[10:]))
	if 12+n > len(raw) {
		return frame{}, errors.New("bad payload length")
	}
	f.data = raw[12 : 12+n]
	return f, nil
}

// request sends a command and waits for the device's response to it,
// answering device-initiated messages that arrive in the meantime.
func (s *session) request(code uint16, data []byte, timeout time.Duration) (frame, error) {
	seq, err := s.send(code, data, 0)
	if err != nil {
		return frame{}, err
	}
	deadline := time.After(timeout)
	for {
		select {
		case f := <-s.frames:
			if f.responseTo == seq {
				return f, nil
			}
			s.handle(f)
		case <-deadline:
			return frame{}, fmt.Errorf("no response to command %#04x", code)
		}
	}
}

// handle answers unsolicited messages from the device: DP reports and time requests.
func (s *session) handle(f frame) {
	now := time.Now()
	_, offset := now.Zone()
	tz := uint16(int16(offset / 36))

	switch f.code {
	case codeReceiveDP, codeReceiveTimeDP:
		dps := f.data
		if f.code == codeReceiveTimeDP {
			dps = skipTimestamp(dps)
		}
		s.reportDPs(dps)
		s.send(f.code, nil, f.seq)
	case codeReceiveSignDP, codeReceiveSignTimeDP:
		if len(f.data) < 3 {
			return
		}
		dps := f.data[3:]
		if f.code == codeReceiveSignTimeDP {
			dps = skipTimestamp(dps)
		}
		s.reportDPs(dps)
		s.send(f.code, []byte{f.data[0], f.data[1], f.data[2], 0}, f.seq) // dp seq, flags, ok
	case codeTime1Req:
		reply := []byte(strconv.FormatInt(now.UnixMilli(), 10))
		s.send(f.code, binary.BigEndian.AppendUint16(reply, tz), f.seq)
	case codeTime2Req:
		reply := []byte{byte(now.Year() % 100), byte(now.Month()), byte(now.Day()),
			byte(now.Hour()), byte(now.Minute()), byte(now.Second()), byte((now.Weekday() + 6) % 7)}
		s.send(f.code, binary.BigEndian.AppendUint16(reply, tz), f.seq)
	}
}

// skipTimestamp drops the leading timestamp of a timed DP report.
func skipTimestamp(data []byte) []byte {
	switch {
	case len(data) >= 14 && data[0] == 0: // 13 ASCII digits, milliseconds
		return data[14:]
	case len(data) >= 5 && data[0] == 1: // 4-byte seconds
		return data[5:]
	}
	return nil
}

func (s *session) reportDPs(data []byte) {
	for len(data) >= 3 {
		id, typ, n := data[0], data[1], int(data[2])
		if len(data) < 3+n {
			return
		}
		if s.onDP != nil {
			s.onDP(id, typ, data[3:3+n])
		}
		data = data[3+n:]
	}
}

// status asks the device for all DP values; it also serves as a liveness check.
func (s *session) status() error {
	_, err := s.request(codeStatus, nil, 10*time.Second)
	return err
}

// login runs the device-info key exchange and pairs with the stored credentials.
func (s *session) login(uuid, deviceID string) error {
	f, err := s.request(codeDeviceInfo, nil, 10*time.Second)
	if err != nil {
		return err
	}
	if len(f.data) < 46 {
		return errors.New("short device info")
	}
	sum := md5.Sum(append(append([]byte{}, s.localKey...), f.data[6:12]...))
	s.sessKey = sum[:]

	req := append([]byte(uuid), s.localKey...)
	req = append(req, deviceID...)
	req = append(req, make([]byte, max(0, 44-len(req)))...)
	f, err = s.request(codePair, req, 10*time.Second)
	if err != nil {
		return err
	}
	if len(f.data) != 1 || (f.data[0] != 0 && f.data[0] != 2) { // 2 = already paired
		return fmt.Errorf("pairing rejected (%x); check uuid / local key / device id", f.data)
	}
	return nil
}

func (s *session) setDP(id, typ byte, value []byte) error {
	data := append([]byte{id, typ, byte(len(value))}, value...)
	_, err := s.request(codeDPs, data, 5*time.Second)
	return err
}

func printDP(id, typ byte, v []byte) {
	switch {
	case typ == dpTypeBool && len(v) == 1:
		fmt.Printf("  DP %d = %v\n", id, v[0] != 0)
	case typ == dpTypeValue && len(v) == 4:
		fmt.Printf("  DP %d = %d\n", id, int32(binary.BigEndian.Uint32(v)))
	case bytes.Count(v, []byte{0}) == len(v):
		fmt.Printf("  DP %d = <%d zero bytes>\n", id, len(v))
	default:
		fmt.Printf("  DP %d = %x\n", id, v)
	}
}

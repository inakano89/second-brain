package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"
)

// MQTTConfig holds broker credentials.
type MQTTConfig struct {
	Broker   string // tcp://host:1883 | tls://host:8883
	ClientID string
	Username string
	Password string
}

func mqttString(s string) []byte {
	b := []byte{byte(len(s) >> 8), byte(len(s))}
	return append(b, s...)
}

func mqttRemaining(n int) []byte {
	var out []byte
	for {
		d := byte(n % 128)
		n /= 128
		if n > 0 {
			d |= 0x80
		}
		out = append(out, d)
		if n == 0 {
			return out
		}
	}
}

// MQTTPublish sends one QoS 0 message using a minimal MQTT 3.1.1 client.
func MQTTPublish(ctx context.Context, cfg MQTTConfig, topic string, payload []byte, retain bool) error {
	if cfg.Broker == "" {
		return errors.New("MQTT_BROKER não configurado")
	}
	u, err := url.Parse(cfg.Broker)
	if err != nil {
		return err
	}
	useTLS := u.Scheme == "tls" || u.Scheme == "ssl" || u.Scheme == "mqtts"
	host := u.Host
	if u.Port() == "" {
		if useTLS {
			host = net.JoinHostPort(u.Hostname(), "8883")
		} else {
			host = net.JoinHostPort(u.Hostname(), "1883")
		}
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	if useTLS {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", host)
	} else {
		conn, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	clientID := cfg.ClientID
	if clientID == "" {
		clientID = "second-brain"
	}
	clientID = fmt.Sprintf("%s-%d", clientID, time.Now().UnixNano()%100000)
	flags := byte(0x02) // clean session
	vh := append(mqttString("MQTT"), 0x04)
	pl := mqttString(clientID)
	if cfg.Username != "" {
		flags |= 0x80
		pl = append(pl, mqttString(cfg.Username)...)
		if cfg.Password != "" {
			flags |= 0x40
			pl = append(pl, mqttString(cfg.Password)...)
		}
	}
	vh = append(vh, flags, 0x00, 0x1E)
	body := append(vh, pl...)
	pkt := append([]byte{0x10}, mqttRemaining(len(body))...)
	if _, err := conn.Write(append(pkt, body...)); err != nil {
		return err
	}
	r := bufio.NewReader(conn)
	ack := make([]byte, 4)
	if _, err := io.ReadFull(r, ack); err != nil {
		return fmt.Errorf("mqtt connack: %w", err)
	}
	if ack[0] != 0x20 || ack[3] != 0 {
		return fmt.Errorf("mqtt: conexão recusada (código %d)", ack[3])
	}
	hdr := byte(0x30)
	if retain {
		hdr |= 0x01
	}
	pub := append(mqttString(topic), payload...)
	msg := append([]byte{hdr}, mqttRemaining(len(pub))...)
	if _, err := conn.Write(append(msg, pub...)); err != nil {
		return err
	}
	_, err = conn.Write([]byte{0xE0, 0x00})
	return err
}

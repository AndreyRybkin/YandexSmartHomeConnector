// Package tv - общее для телевизоров в домашней сети: имена функций телевизора как прибора
// по профилю и пакет Wake-on-LAN, которым шлюз включает выключенный телевизор.
package tv

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Имена функций телевизора. Профили телевизоров LG и Samsung берут их одинаково.
const (
	// Power - питание: on или off.
	Power = "TV.Power"
	// Volume - громкость от 0 до 100.
	Volume = "TV.Volume"
	// Mute - звук выключен: true или false.
	Mute = "TV.Mute"
	// Channel - номер канала.
	Channel = "TV.Channel"
	// Input - источник сигнала: вход телевизора, например HDMI_1, или эфир LIVE_TV.
	Input = "TV.Input"
	// Playback - воспроизведение: playing или paused.
	Playback = "TV.Playback"
	// App - приложение на экране, только для чтения.
	App = "TV.App"
)

// Значения функций телевизора.
const (
	On      = "on"
	Off     = "off"
	Playing = "playing"
	Paused  = "paused"
	// LiveTV - эфирное телевидение как источник сигнала.
	LiveTV = "LIVE_TV"
)

// MaxChannel - самый большой номер канала, который шлюз отправляет телевизору.
const MaxChannel = 9999

// MaxSteps - предел шагов громкости или канала одной командой: «громче на 50» кнопками
// пульта шла бы минуту.
const MaxSteps = 20

// DefaultBroadcast - куда шлюз шлёт пакет Wake-on-LAN, если адрес не задан.
const DefaultBroadcast = "255.255.255.255"

// wolPort - порт пакета Wake-on-LAN.
const wolPort = "9"

// ErrNoMAC - MAC телевизора не известен: включить его по сети нельзя.
var ErrNoMAC = errors.New("MAC телевизора не известен: шлюз узнаёт его у включённого телевизора, его можно задать в настройках")

// ParseMAC разбирает MAC-адрес вида AA:BB:CC:DD:EE:FF или AA-BB-CC-DD-EE-FF.
func ParseMAC(s string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(strings.TrimSpace(s))
	if err != nil || len(mac) != 6 {
		return nil, fmt.Errorf("MAC %q: нужен адрес вида AA:BB:CC:DD:EE:FF", s)
	}
	return mac, nil
}

// Packet - пакет Wake-on-LAN: 6 байт 0xFF и 16 раз MAC.
func Packet(mac net.HardwareAddr) []byte {
	out := make([]byte, 0, 6+16*len(mac))
	for range 6 {
		out = append(out, 0xFF)
	}
	for range 16 {
		out = append(out, mac...)
	}
	return out
}

// Wake отправляет пакет Wake-on-LAN на широковещательный адрес broadcast и на адрес
// телевизора host по UDP на порт 9. Пакет на адрес телевизора доходит и из контейнера в
// сети Docker, если сеть ещё помнит телевизор; широковещательный - только из сети хоста.
func Wake(mac net.HardwareAddr, broadcast, host string) error {
	if len(mac) == 0 {
		return ErrNoMAC
	}
	if broadcast == "" {
		broadcast = DefaultBroadcast
	}
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	packet := Packet(mac)
	var sent int
	var last error
	for _, h := range []string{broadcast, hostOnly(host)} {
		if h == "" {
			continue
		}
		addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(h, wolPort))
		if err == nil {
			_, err = conn.WriteTo(packet, addr)
		}
		if err != nil {
			last = err
			continue
		}
		sent++
	}
	if sent == 0 {
		return fmt.Errorf("пакет Wake-on-LAN не отправлен: %w", last)
	}
	return nil
}

// hostOnly - адрес без порта.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}

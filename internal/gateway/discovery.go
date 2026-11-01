package gateway

import (
	"context"
	"net/netip"

	"hcgateway/internal/config"
	"hcgateway/internal/discovery"
)

// maxCoffeeLookups - сколько кофемашин аккаунта поиск спрашивает об их адресе у облака.
const maxCoffeeLookups = 10

// CoffeeAddresses - адреса кофемашин аккаунта De'Longhi, которые машины сами сообщили облаку:
// по ним поиск узнаёт, какая машина нашлась. Без входа в аккаунт - пусто.
func (g *Gateway) CoffeeAddresses(ctx context.Context) map[netip.Addr]discovery.Found {
	out := map[netip.Addr]discovery.Found{}
	if !g.DeLonghi.Status().LoggedIn {
		return out
	}
	for i, d := range g.DeLonghi.Devices() {
		if i == maxCoffeeLookups {
			break
		}
		ip, err := g.DeLonghi.LanAddress(ctx, d.MachineName)
		if err != nil {
			g.log.Debug("De'Longhi: адрес машины не получен", "serial", d.Serial, "err", err)
			continue
		}
		if a, err := netip.ParseAddr(ip); err == nil {
			out[a] = discovery.Found{Source: config.SourceDeLonghi, Serial: d.Serial, Model: d.Model}
		}
	}
	return out
}

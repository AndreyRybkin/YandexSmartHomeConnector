package hccloud

// Region - облако Home Connect, в котором живёт аккаунт: облака разных регионов не связаны.
type Region struct {
	// Name - имя региона в команде fetch-keys и в форме интерфейса.
	Name string
	// Base - адрес входа и API приборов.
	Base string
	// Hosts - серверы аккаунтов региона, перебираются по порядку.
	Hosts []string
}

const (
	euHost = "https://eu.services.home-connect.com"
	naHost = "https://na.services.home-connect.com"
)

var (
	// EU - Европа.
	EU = Region{Name: "eu", Base: "https://api.home-connect.com", Hosts: []string{euHost}}
	// NA - Северная Америка.
	NA = Region{Name: "na", Base: "https://api-rna.home-connect.com", Hosts: []string{naHost}}
	// China - материковый Китай.
	China = Region{Name: "cn", Base: "https://api.home-connect.cn", Hosts: []string{"https://cn.services.home-connect.cn"}}
	// Global - вход через общий адрес, аккаунт ищется в Европе и в Северной Америке.
	Global = Region{Name: "global", Base: "https://api.home-connect.com", Hosts: []string{euHost, naHost}}
)

// RegionByName находит регион по имени; пустое имя - Global.
func RegionByName(name string) (Region, bool) {
	switch name {
	case "", Global.Name:
		return Global, true
	case EU.Name:
		return EU, true
	case NA.Name:
		return NA, true
	case China.Name:
		return China, true
	}
	return Region{}, false
}

package hclocal

import (
	"crypto/aes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"hcgateway/internal/hcdesc"
)

const (
	// Файлы прибора в data_dir/appliances/<haId>: их пишет команда fetch-keys.
	keyFileName     = "appliance.json"
	mappingFile     = "FeatureMapping.xml"
	descriptionFile = "DeviceDescription.xml"
	// maxKeyFile и maxXML - пределы файлов прибора.
	maxKeyFile = 64 << 10
	maxXML     = 16 << 20
)

// Режимы связи из appliance.json.
const (
	// ModeTLS - WebSocket по TLS-PSK, порт 443.
	ModeTLS = "tls"
	// ModeAES - WebSocket без TLS, сообщения зашифрованы AES-CBC с HMAC, порт 80.
	ModeAES = "aes"
)

// Info - сведения о приборе из appliance.json.
type Info struct {
	HaID   string `json:"haId"`
	Brand  string `json:"brand"`
	HaType string `json:"haType"`
	Mode   string `json:"mode"`
}

// keyFile - appliance.json: сведения о приборе и ключи для связи с ним.
type keyFile struct {
	Info
	Key string `json:"key"`
	IV  string `json:"iv,omitempty"`
}

// keys - ключи прибора: PSK, а в режиме AES ещё IV.
type keys struct {
	mode    string
	psk, iv []byte
}

// readLimited читает файл не больше limit байт.
func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s больше %d байт", filepath.Base(path), limit)
	}
	return data, nil
}

func readKeyFile(dir string) (*keyFile, error) {
	data, err := readLimited(filepath.Join(dir, keyFileName), maxKeyFile)
	if err != nil {
		return nil, err
	}
	var f keyFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s не разобран: %w", keyFileName, err)
	}
	return &f, nil
}

// decodeKey - ключ из облака: base64 для адресов, с дополнением = или без.
func decodeKey(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// load читает ключи и описание прибора haID из каталога dir.
func load(dir, haID string) (Info, keys, *hcdesc.Description, error) {
	f, err := readKeyFile(dir)
	if err != nil {
		return Info{}, keys{}, nil, err
	}
	if f.HaID != haID {
		return Info{}, keys{}, nil, fmt.Errorf("%s: haId %q, а нужен %q", keyFileName, f.HaID, haID)
	}
	if f.HaType == "" {
		return Info{}, keys{}, nil, fmt.Errorf("%s: не задан haType", keyFileName)
	}
	k := keys{mode: f.Mode}
	if k.psk, err = decodeKey(f.Key); err != nil || len(k.psk) == 0 {
		return Info{}, keys{}, nil, fmt.Errorf("%s: ключ не разобран", keyFileName)
	}
	switch f.Mode {
	case ModeTLS:
	case ModeAES:
		if k.iv, err = decodeKey(f.IV); err != nil || len(k.iv) != aes.BlockSize {
			return Info{}, keys{}, nil, fmt.Errorf("%s: iv не разобран, нужно %d байт", keyFileName, aes.BlockSize)
		}
	default:
		return Info{}, keys{}, nil, fmt.Errorf("%s: mode %q, нужно %s или %s", keyFileName, f.Mode, ModeTLS, ModeAES)
	}
	fm, err := readLimited(filepath.Join(dir, mappingFile), maxXML)
	if err != nil {
		return Info{}, keys{}, nil, err
	}
	dd, err := readLimited(filepath.Join(dir, descriptionFile), maxXML)
	if err != nil {
		return Info{}, keys{}, nil, err
	}
	desc, err := hcdesc.Parse(fm, dd)
	if err != nil {
		return Info{}, keys{}, nil, err
	}
	return f.Info, k, desc, nil
}

// List - приборы, ключи которых лежат в каталоге appliances: по каталогу на haId.
// Каталоги без appliance.json или с чужим haId пропускаются.
func List(appliances string) []Info {
	entries, err := os.ReadDir(appliances)
	if err != nil {
		return nil
	}
	var out []Info
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		f, err := readKeyFile(filepath.Join(appliances, e.Name()))
		if err == nil && f.HaID == e.Name() {
			out = append(out, f.Info)
		}
	}
	slices.SortFunc(out, func(a, b Info) int { return strings.Compare(a.HaID, b.HaID) })
	return out
}

// errNoKeys - у прибора нет ключей: их загружает команда fetch-keys.
var errNoKeys = errors.New("нет ключей прибора: загрузите их командой hcgateway fetch-keys")

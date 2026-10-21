package plugin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// 用户在「模型来源」里取消勾选的模型，按来源记在 models-off.json：{"<来源>": ["<模型>", …]}。
// 记取消的而不是勾选的，来源新出的模型默认可用。来源 codex 指 codex 订阅自己的模型。

var offMu sync.Mutex

func offPath() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "models-off.json"), nil
}

// OffModels 读全部来源取消勾选的模型。文件不存在时返回空表。
func OffModels() map[string][]string {
	offMu.Lock()
	defer offMu.Unlock()
	return readOff()
}

func readOff() map[string][]string {
	out := map[string][]string{}
	p, err := offPath()
	if err != nil {
		return out
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return out
	}
	json.Unmarshal(b, &out)
	return out
}

// ModelOff 判断一个来源的模型是不是被取消勾选了。
func ModelOff(source, id string) bool {
	return slices.Contains(OffModels()[source], id)
}

// SetModelsOff 替换一个来源取消勾选的模型；ids 为空时删掉这个来源的记录。
func SetModelsOff(source string, ids []string) error {
	if source == "" {
		return errors.New("缺来源")
	}
	offMu.Lock()
	defer offMu.Unlock()
	m := readOff()
	if len(ids) == 0 {
		delete(m, source)
	} else {
		m[source] = slices.Compact(slices.Sorted(slices.Values(ids)))
	}
	p, err := offPath()
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

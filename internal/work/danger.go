package work

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"morula/internal/journal"
	"morula/internal/store"
)

// 危险操作清单：子 agent 碰到这些操作先上报，用户批准后才做。
// 清单存在数据目录的 approval-rules.txt，一行一条，# 开头的行是注释。
// 用户可以在设置里改，也可以直接改文件；每次给子 agent 写提示词时重新读。

const dangerFile = "approval-rules.txt"

// DefaultDanger 是文件不存在时写进去的清单。
var DefaultDanger = []string{
	"删除文件或目录",
	"改写或删除 git 历史、强制推送",
	"删除数据库表或数据",
	"修改项目文件夹以外的文件",
	"向外部服务发送内容",
	"删除或修改 agent（包括 morula 的团队）",
}

const dangerHead = "# morula 的危险操作清单：子 agent 碰到下面这些操作先停下来问你，你批准后才做。\n" +
	"# 一行一条，# 开头的行不算。改完保存即可，下一个开工的任务就按新清单。\n"

// DangerPath 返回清单文件的路径。
func DangerPath() (string, error) {
	d, err := store.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, dangerFile), nil
}

// Danger 读清单。文件不存在时写入默认清单再返回；读不了时返回默认清单和错误。
func Danger() ([]string, error) {
	p, err := DangerPath()
	if err != nil {
		return DefaultDanger, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return DefaultDanger, SaveDanger(DefaultDanger)
	}
	if err != nil {
		return DefaultDanger, err
	}
	out := []string{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out, nil
}

// SaveDanger 写清单。空行和重复的条目去掉。
func SaveDanger(rules []string) error {
	p, err := DangerPath()
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(dangerHead)
	seen := map[string]bool{}
	for _, r := range rules {
		r = strings.Join(strings.Fields(r), " ")
		if r == "" || strings.HasPrefix(r, "#") || seen[r] {
			continue
		}
		seen[r] = true
		b.WriteString(r + "\n")
	}
	return os.WriteFile(p, []byte(b.String()), 0o644)
}

// dangerRule 是子 agent 规则里的第 3 条。清单为空时照样写出，告诉子 agent 不必先问。
func dangerRule() string {
	list, _ := Danger()
	if len(list) == 0 {
		return "3. 用户没有列危险操作，不必先上报。"
	}
	return "3. 危险操作先不要做：" + strings.Join(list, "；") +
		"。需要时 status=approval，approval.action 写具体要执行的操作，approval.reason 写原因；用户批准后你会收到通知。"
}

// 审批档位，输入框旁的菜单里选：ask 每次都问，list 按清单问（默认），all 全部放行。
// 选了就一直有效，直到用户再改。设置项存成「日期|档位」，日期是改档位的那天，只作记录。
// 子 agent 开工时读一次，开工后改档位不影响它。
const (
	TierAsk  = "ask"
	TierList = "list"
	TierAll  = "all"
)

// Tier 是当前的审批档位。
func (e *Engine) Tier() string {
	v := strings.Split(e.DB.Setting("approval.tier", ""), "|")
	if len(v) == 2 && (v[1] == TierAsk || v[1] == TierAll) {
		return v[1]
	}
	return TierList
}

// SetTier 改审批档位。
func (e *Engine) SetTier(t string) error {
	if t != TierAsk && t != TierList && t != TierAll {
		return errors.New("没有这个审批档位：" + t)
	}
	return e.DB.SetSetting("approval.tier", journal.Today()+"|"+t)
}

// tierRule 是子 agent 规则里的第 3 条，按审批档位写。
func tierRule(tier string) string {
	switch tier {
	case TierAsk:
		return "3. 用户选了「每次都问」：写文件、跑会改东西的命令、联网之前都先不要做，status=approval，approval.action 写具体要执行的操作，" +
			"approval.reason 写原因；用户批准后你会收到通知。只读的命令（看文件、git status、git diff）不用问。"
	case TierAll:
		return "3. 用户选了「全部放行」：危险操作清单上的事直接做，改工作目录以外的文件、联网也直接做，summary 里写明做了哪些。" + alwaysRule()
	}
	return dangerRule() + alwaysRule()
}

// alwaysRule 接在第 3 条后面：AlwaysAsk 里的操作在任何档位都先上报，用户从清单里删掉也照样要问。
func alwaysRule() string {
	return "下面这些在任何档位都先不要做，status=approval，approval.action 写具体要执行的操作，approval.reason 写原因，用户批准后再做：" + strings.Join(AlwaysAsk, "；") +
		"。任务说明里写明要删、用户原话里说了要删也一样：用户在审批卡片上批准了才算数，先上报再做。"
}

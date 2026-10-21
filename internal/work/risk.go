package work

import (
	"path/filepath"
	"regexp"
	"strings"
)

// 助理用 run_command 提出的命令按审批档位决定问不问：
// 每次都问时每条都问；按清单问时只读的命令直接跑，其余要问；全部放行时直接跑，只有高危命令要问。
// 高危命令（删除文件、改写 git 历史、清数据库）在任何档位都要问，判断见 riskOf。

// AlwaysAsk 是任何档位都要用户批准的操作，子 agent 的第 3 条规则里也照这份写。
var AlwaysAsk = []string{
	"删除文件或目录（包括移到废纸篓、清空目录）",
	"改写或删除 git 历史、强制推送、丢掉没提交的改动",
	"删除数据库表或数据",
}

// cmdSplit 把一行命令拆成几段：管道、分号、&&、||、换行、子 shell。
var cmdSplit = regexp.MustCompile("\\|\\||&&|[|;&\\n()`]|\\$\\(")

// fdDup 是 2>&1、>&2、&> 这类改道，拆段前去掉，免得 & 被当成分隔符。
var fdDup = regexp.MustCompile(`[0-9]*>>?&[0-9-]+|&>>?`)

// segments 拆出每一段的词，去掉开头的环境变量赋值和不改变意思的前缀（env、command、nohup、time、xargs 等）。
func segments(cmd string) [][]string {
	var out [][]string
	cmd = fdDup.ReplaceAllString(cmd, " ")
	for _, part := range cmdSplit.Split(cmd, -1) {
		f := strings.Fields(part)
		for len(f) > 0 {
			w := f[0]
			if strings.Contains(w, "=") && !strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "=") {
				f = f[1:]
				continue
			}
			switch filepath.Base(strings.Trim(w, `"'`)) {
			case "env", "command", "builtin", "nohup", "time", "nice", "exec", "xargs", "caffeinate":
				f = f[1:]
				for len(f) > 0 && strings.HasPrefix(f[0], "-") {
					f = f[1:]
				}
				continue
			}
			break
		}
		if len(f) > 0 {
			f[0] = filepath.Base(strings.Trim(f[0], `"'`))
			out = append(out, f)
		}
	}
	return out
}

func has(f []string, flags ...string) bool {
	for _, w := range f[1:] {
		for _, fl := range flags {
			if w == fl || (strings.HasPrefix(fl, "--") && strings.HasPrefix(w, fl+"=")) {
				return true
			}
		}
	}
	return false
}

// 写在代码里的删除：python、node、osascript 等一行脚本。
var scriptDelete = regexp.MustCompile(`(?i)rmtree|os\.remove|os\.unlink|\.unlink\(|os\.rmdir|fs\.rm|unlinkSync|rimraf|to delete\b|delete (file|folder|every|items?)\b|empty trash`)

var sqlDelete = regexp.MustCompile(`(?i)\bdrop\s+(table|database|schema|index|view)\b|\btruncate\s+(table\s+)?\w|\bdelete\s+from\b`)

// riskOf 返回命令为什么算高危；不是高危时返回空串。按词判断，判断不了的写法（例如变量拼出来的命令）不算高危，
// 所以子 agent 和助理的规则里仍写明删除前要先问。
func riskOf(cmd string) string {
	if scriptDelete.MatchString(cmd) {
		return "会删除文件"
	}
	if sqlDelete.MatchString(cmd) {
		return "会删除数据库里的表或数据"
	}
	for _, f := range segments(cmd) {
		switch f[0] {
		case "rm", "rmdir", "unlink", "srm", "shred", "trash":
			return "会删除文件（" + f[0] + "）"
		case "sudo", "doas":
			return "要用管理员权限执行"
		case "dd", "mkfs", "newfs_apfs", "newfs_hfs":
			return "会覆盖磁盘或文件（" + f[0] + "）"
		case "diskutil":
			if len(f) > 1 && (strings.HasPrefix(f[1], "erase") || strings.HasPrefix(f[1], "zero") || f[1] == "partitionDisk" || f[1] == "apfs") {
				return "会抹掉磁盘（diskutil " + f[1] + "）"
			}
		case "find":
			if has(f, "-delete") || (has(f, "-exec", "-execdir", "-ok") && strings.Contains(strings.Join(f, " "), " rm")) {
				return "会删除找到的文件（find）"
			}
		case "rsync":
			for _, w := range f[1:] {
				if strings.HasPrefix(w, "--delete") || w == "--remove-source-files" {
					return "会删除文件（rsync " + w + "）"
				}
			}
		case "mv":
			if strings.Contains(strings.Join(f, " "), ".Trash") {
				return "会把文件移到废纸篓"
			}
		case "git":
			if r := gitRisk(f); r != "" {
				return r
			}
		}
	}
	return ""
}

func gitRisk(f []string) string {
	// 跳过 -C 目录、-c 配置这类全局参数，找到子命令
	i := 1
	for i < len(f) && strings.HasPrefix(f[i], "-") {
		if f[i] == "-C" || f[i] == "-c" {
			i++
		}
		i++
	}
	if i >= len(f) {
		return ""
	}
	sub, rest := f[i], f[i:]
	switch sub {
	case "push":
		if has(rest, "-f", "--force", "--force-with-lease", "--force-if-includes", "--delete", "-d", "--mirror", "--prune") {
			return "会改写或删除远端的 git 历史（git push）"
		}
		for _, w := range rest[1:] {
			if strings.HasPrefix(w, "+") || strings.HasPrefix(w, ":") {
				return "会改写或删除远端的 git 历史（git push " + w + "）"
			}
		}
	case "reset":
		if has(rest, "--hard", "--merge", "--keep") {
			return "会丢掉没提交的改动（git reset）"
		}
	case "clean":
		return "会删除没纳入版本管理的文件（git clean）"
	case "checkout":
		if has(rest, "--", ".", "-f", "--force") {
			return "会丢掉没提交的改动（git checkout）"
		}
	case "restore":
		return "会丢掉没提交的改动（git restore）"
	case "branch":
		if has(rest, "-D", "-d", "--delete", "-M", "-f", "--force") {
			return "会删除或覆盖分支（git branch）"
		}
	case "tag":
		if has(rest, "-d", "--delete", "-f", "--force") {
			return "会删除或覆盖标签（git tag）"
		}
	case "stash":
		if len(rest) > 1 && (rest[1] == "drop" || rest[1] == "clear") {
			return "会删除暂存的改动（git stash " + rest[1] + "）"
		}
	case "rebase", "filter-branch", "filter-repo", "replace":
		return "会改写 git 历史（git " + sub + "）"
	case "commit":
		if has(rest, "--amend") {
			return "会改写 git 历史（git commit --amend）"
		}
	case "reflog":
		if len(rest) > 1 && (rest[1] == "expire" || rest[1] == "delete") {
			return "会删除 git 历史记录（git reflog）"
		}
	case "update-ref":
		if has(rest, "-d") {
			return "会删除 git 引用（git update-ref -d）"
		}
	case "gc", "prune":
		return "会清掉 git 里没被引用的历史（git " + sub + "）"
	case "rm":
		return "会删除文件（git rm）"
	}
	return ""
}

// readOnlyCmds 是只看不改的命令。git、find、sed 等另看参数。
var readOnlyCmds = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "grep": true, "egrep": true, "fgrep": true, "rg": true,
	"wc": true, "du": true, "df": true, "pwd": true, "echo": true, "printf": true, "which": true, "whereis": true,
	"type": true, "file": true, "stat": true, "ps": true, "pgrep": true, "uname": true, "sw_vers": true, "date": true,
	"id": true, "whoami": true, "mdfind": true, "mdls": true, "tree": true, "realpath": true, "readlink": true,
	"dirname": true, "basename": true, "sort": true, "uniq": true, "cut": true, "tr": true, "jq": true, "diff": true,
	"cmp": true, "shasum": true, "md5": true, "otool": true, "lsof": true, "system_profiler": true, "true": true,
	"test": true, "[": true, "hostname": true, "uptime": true, "vm_stat": true, "xattr": true, "column": true, "nl": true,
}

var gitReadOnly = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "rev-parse": true, "ls-files": true, "blame": true,
	"describe": true, "shortlog": true, "ls-tree": true, "cat-file": true, "rev-list": true, "grep": true,
}

// 输出改道到文件：> 和 >>，不算 >&2、2>&1、>/dev/null。
var redirect = regexp.MustCompile(`>>?\s*([^\s&]+)`)

// readOnly 判断命令是不是只看不改。判断不了的都算会改东西。
func readOnly(cmd string) bool {
	for _, m := range redirect.FindAllStringSubmatch(cmd, -1) {
		if m[1] != "/dev/null" {
			return false
		}
	}
	segs := segments(cmd)
	if len(segs) == 0 {
		return false
	}
	for _, f := range segs {
		switch {
		case readOnlyCmds[f[0]]:
			if f[0] == "xattr" && has(f, "-d", "-c", "-w", "-r") {
				return false
			}
		case f[0] == "find":
			if has(f, "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprintf", "-fls") {
				return false
			}
		case f[0] == "sed":
			if has(f, "-i") || strings.Contains(strings.Join(f, " "), " -i") {
				return false
			}
		case f[0] == "defaults":
			if len(f) < 2 || (f[1] != "read" && f[1] != "read-type" && f[1] != "domains") {
				return false
			}
		case f[0] == "plutil":
			if !has(f, "-p", "-lint") {
				return false
			}
		case f[0] == "git":
			i := 1
			for i < len(f) && strings.HasPrefix(f[i], "-") {
				if f[i] == "-C" {
					i++
				}
				i++
			}
			if i >= len(f) || !gitReadOnly[f[i]] {
				if i < len(f) && f[i] == "branch" && !has(f[i:], "-D", "-d", "--delete", "-m", "-M", "-c", "-C", "-f", "--force", "-u", "--set-upstream-to") {
					continue
				}
				if i < len(f) && f[i] == "remote" && (len(f) == i+1 || f[i+1] == "-v" || f[i+1] == "show" || f[i+1] == "get-url") {
					continue
				}
				if i < len(f) && f[i] == "config" && has(f[i:], "--get", "--list", "-l", "--get-all") {
					continue
				}
				return false
			}
		default:
			return false
		}
	}
	return true
}

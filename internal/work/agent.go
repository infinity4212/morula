package work

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"morula/internal/chat"
	"morula/internal/harness"
	"morula/internal/journal"
	"morula/internal/store"
)

// agent 方式的助理：一个续跑的 harness 会话，
// 自己调 morula 给的工具查项目、问用户、建项目、派活，最后一条消息是给用户的回复。
// harness 不能接工具或不能续跑时，Turn 仍走判断归属、安排两步。

const agentRole = `你是 morula 里的助理。用户是管理者，不是执行者：用户只跟你说话，你判断每条消息属于哪个项目，
自己能做的自己做，一两条命令能做完的小事经用户批准后自己做，要改文件的活派给子 agent，子 agent 做完后 morula 检查并向用户汇报。
回答用中文，第一句是结论，简洁。不编造事实、数字、路径。
跟用户说话用日常的说法，不说 harness、token、MCP、沙箱、进程、退出码、JSON、会话 id、子 agent 这类词：harness 说「AI 工具」，子 agent 说 agent 或它的名字，会话说「对话」。用户自己用了这些词、或问到技术细节时照用户的说法答。文件路径、命令要给用户看时照原样写。

# 你能做什么
你在一个只读沙箱里：可以读本机的文件、运行只读的命令查情况，不能直接改文件。要改东西时用 run_command 或 dispatch（见下面「怎么干活」）。
沙箱还会挡住启动应用、监听端口、查进程和用 Spotlight 搜文件：在沙箱里跑 open、pgrep、ps、lsappinfo、mdfind、启动服务时，
报 Operation not permitted、kLSNoExecutableErr、绑定端口被拒、没有输出，原因都是沙箱，不是应用坏了或文件不存在。
不要据此下结论，改用 run_command 执行同一条命令（找文件改用 find、ls 这类只读命令）。
不要用 computer-use、cua 这类操作屏幕的工具，也不要用 harness 自带的 send_message、spawn_agent 之类的多 agent 工具：派活和转接只用下面 morula 的工具。
morula 给了你这些工具（工具名前面可能带服务器名 morula_assistant）：
- list_projects：全部项目的编号、名字和项目摘要。每条消息后面附了一份当时的项目列表，够用时不用再调。
- list_agents：团队里的 agent，写着每个 agent 用的 harness、模型和思考强度。固定成员是你自己（助理）和验收；派活时从子 agent 里选。
  用户问团队里有谁时固定成员也要算上。每条消息后面附了一份当时的团队名单，够用时不用再调。
- list_harnesses：本机的 harness 哪些能用、各有哪些模型和思考强度。每条消息后面附了一份，模型多的 harness 只列了前面一部分，要看全部时再调。
- read_project：一个项目的工作目录、项目记忆、花名册和最近几次安排。要在某个项目里干活前先读。
- ask_user：发一张问题卡片问用户，等用户回答。
- create_project：新建项目。
- rename_project：给项目改名。
- merge_projects：合并两个项目，用户在卡片上确认后由 morula 合并。
- undo_merge：撤销一次合并。
- move_runs：把几次安排挪到另一个项目。
- dispatch：在一个项目里派任务给子 agent。马上返回，不等做完。
- propose_team_change：增删改团队里的 agent，用户在卡片上确认后由 morula 改。
- update_project：改项目记忆或项目摘要。
- delete_project：删掉一个项目在 morula 里的记录，用户在卡片上确认后由 morula 删。
- list_routines、save_routine、remove_routine：查、新建或修改、删掉例行事项（到点由你照原话处理的事）。删掉和关掉开着的事项，用户在卡片上确认后才改。
- transfer：把要和用户来回商量的事转接给一个 agent，开始用户和它的对话。
- run_command：提出一条要在沙箱外执行的 shell 命令，由 morula 执行，返回退出码和输出。要不要先等用户批准看用户选的审批档位，结果里会写明；删除文件、改写 git 历史、清数据库这类命令在任何档位都要用户批准。
- update_memory：改全局记忆（跨项目都适用的用户偏好、约定、习惯）。每条新会话开头附了当前的全局记忆。
- search_chats：按关键词找以前的对话，包括 morula 里的对话、本机 codex 等工具的会话，以及 morula 派给子 agent 干活的会话。
  用户问以前聊过、做过什么，或提到「之前那个」「上次」时先用它找，找到后读那个文件，不要说没有历史记录。
  问某个 agent 跑过的会话、做到哪了时填 agent。命中的会话可能只是提到了这个词，是谁跑的看结果里写的 agent，不要张冠李戴。

# 怎么干活
- 分项目、建项目、改名、合并是你的杂活，不要让用户来分，也不要为这些事问用户：
  - 用户点名了项目（例如「放到官网改版」）：就用那个项目，不用猜。
  - 能确定属于哪个项目就直接用。拿不准时放进最可能的那个项目；哪个都不像时放进默认项目。不要用 ask_user 问属于哪个项目。
    放进了哪个项目，用户在回复上看得到，觉得不对会点一下改或说一句。
  - 用户说放错了项目（例如「这件事应该放在「X」」）：以后按用户说的放；这件事派过活的，用 move_runs 把那次安排挪过去；
    已经写进原项目记忆的结论，挪进对的项目记忆。回复里用一句话说挪了什么。
  打招呼、闲聊、问你能做什么、问团队和项目有哪些、不属于任何项目的零散小事：算默认项目（列表里标了「默认项目」），直接回答，
  不用问用户，也不新建项目。零散小事要改东西时，按下面 run_command 和 dispatch 的分工做；dispatch 时派到默认项目里。
- 明显是一件新的、以后还要接着做的事：用 create_project 直接建，不问用户起什么名。name 按这件事长期要做的工作起，
  参考全局记忆、对话和用户自己的叫法，简短（一般不超过 10 个字）；附件的文件名只说明这次发来了什么，不要直接拿来当项目名。
  用户给了名字时（例如「新建为 CRO 项目」）用用户的名字。create_project 返回了项目编号，回复里再用一句话说建了哪个项目；
  没调用或调用失败时不要说建了。
- 项目名不再贴切（做的事变了、用户换了叫法）：用 rename_project 直接改，改成了再在回复里说一句。
- 两个项目其实是一件事：用 merge_projects 提出合并，用户在卡片上确认。用户说合并错了：用 undo_merge 撤销。
- 默认项目里同一类的零散小事出现了 3 次以上（read_project 默认项目时看最近的安排）：在回复末尾用一句话问用户要不要单独建一个项目。
  用户同意后用 create_project 建，再用 move_runs 把这几次安排挪进去。
- 只要产出文字、不用改文件的活（出方案、写分析、回答问题、问项目进展、改写或检查一段文字）：你自己做，写在回复里。需要看文件时自己读。
  这类活不 dispatch：子 agent 看不到这段对话，验收也会因为没有文件产出打回。
- 一两条命令就能做完、不用改代码的小事（打开应用、去掉 quarantine 隔离标记、建一个目录、复制或移动一个文件、结束一个进程、改一项系统设置）：
  先用只读命令查清楚（例如用 ls /Applications 查应用装在哪、名字对不对），再用 run_command 提出要执行的命令，reason 写清会改变什么。
  打开应用、查进程不要先在沙箱里试 open、pgrep：一定会被挡，直接用 run_command。
  退出码 0 不等于做成了：执行后再用只读命令核对（例如 open 后用 pgrep 或 lsappinfo 查进程），核对不了的（例如有没有弹出系统拦截窗口）写未验证，
  请用户看一眼。用户拒绝了就不要换个写法再执行同样的操作。这类小事不 dispatch，也不新建项目。
- 要改代码、改好几个文件，或活很大要分几步：用 dispatch 派下去。每个任务的 brief 写清楚目标、产出和验收标准，
  让没看过这段对话的子 agent 也能独立完成；用户发来的附件路径要写进 brief，子 agent 看不到这段对话。
  agent 填花名册里合适的执行者，没有合适的填空串，morula 会安排一个通用执行者。
  check 填一条在项目目录里运行、退出码 0 表示通过的检查命令，没有填空串。
  effort 按活的难度定思考强度，越高越慢：复制文件、装 skill、跑一条命令、照着写好的步骤改用 low；
  一般的改代码、写文档用 medium；要查原因、改好几处互相牵连的代码、出没有现成做法的方案用 high；
  更高的档（xhigh、max 等）只在用户要求，或同一件活低档做失败、被打回过时用。拿不准填空串，用执行者自己的设置（花名册里写着）。
  派出去以后活还没做完：回复里说派了什么、由谁做，不要说已经做完。子 agent 做完后验收 agent 会核对，没通过会打回给你重新安排。
  派完就回复用户，不要等活做完，也不要用 read_project 反复查进度：用户在等你回复，做完后 morula 会另外汇报。
  用户这条消息里还有要等这批活做完才能做的事（例如「装好这个 skill，再用它建一个 agent」）：写进 dispatch 的 after，
  morula 汇报后交回给你接着做。回复里说做完后会接着做什么。
- 要和用户来回商量才能做完的事（追问方案、访谈、一起改稿，例如用 skill grill-me）：用 transfer 转接给合适的 agent，
  不要 dispatch。子 agent 一次性运行，问不了用户；也不要你自己替它向用户提问。brief 写清用户要什么、背景、要用的 skill。
  没有合适的 agent（例如没有绑这个 skill 的）时，先用 propose_team_change 建一个，再转接。
- 项目有项目负责人时（项目列表里写着），dispatch 到这个项目的活整件交给负责人：你写的任务是给它的建议拆法，
  由它决定怎么拆、检查子任务、写汇报和项目记忆。你照常写清要求和拆法。
- 用户直接和某个 agent 对话过时，「会话外发生的事」里有它写的纪要，纪要也在项目记忆里。
- 子 agent 或验收说某个工具用不了、某个目录或文件读不到时，先用你自己的只读命令核对一次（例如 ls 那个目录），再决定怎么安排。
  核对后能读到：照原来的要求重新派，brief 里写明你核对的命令和结果；不要改成绕开这个目录或降低要求的做法。核对后确实读不到：告诉用户，问怎么办。
- 增删改团队里的 agent 是管理 morula 本身，不属于任何项目：用 propose_team_change，不要新建项目，也不要派活。
  要让 agent 用某个 skill 时，在 skills 里填 skill 的名字。skill 还没装好时先派活去装，建 agent 写进 after。
  新建或修改 agent 时可以定 runtime（harness）、model、effort，只能从「本机的 harness 和模型」里选能用的：
  用户点名了 harness 或模型就照用户的；没点名时不填，和助理相同。不要编造列表里没有的模型。
  名字对不上、或拿不准是哪一个时先问用户。
- 用户要删项目：用 delete_project，用户在卡片上确认。两个项目其实是一件事时用 merge_projects 合并，不要删。删不了（默认项目、有安排在跑）时照结果告诉用户。
- 用户要每天、每周固定时间做的事（例如「每天早上 9 点汇总邮件」）：用 save_routine 建例行事项，不要派活，也不要新建项目。
  prompt 写成到点时你收到就能照做的原话；用户点名了交给谁、放进哪个项目就填上。时间、星期几说得不清楚时问用户。
  改、关、删之前先用 list_routines 查编号。用户只说「先停一下」时用 save_routine 关掉，不要删。
- 用户要你记住的偏好和习惯、跨项目都适用的约定：用 update_memory 写进全局记忆。只属于一个项目的事用 update_project。
  用户说「记到全局记忆」时就写全局记忆，不要改写到项目记忆里。
- 项目里有了以后还用得上的结论或决定：用 update_project 改项目记忆。项目摘要只有一句话，说明项目是什么，
  你以后靠它判断消息归不归这个项目；进展写进项目记忆，不写进摘要。默认项目的摘要不改。
- 只在这些时候问用户：管理团队、合并项目（都由卡片确认）、要用户拍板的事。分项目、建项目、改项目名、选哪个执行者、怎么拆任务，你自己定。
- 收到消息后，先用一句话说你接下来要做什么（例如「我读一下这个方案」），再查文件、调工具；用户会马上看到这句。
  能直接回答的（打招呼、问团队有谁、问你能做什么）不用这句，直接写回复。
- 能自己查清的事查完再回复：回复里不要写「还没检查 X」「需要的话我可以再查 Y」，能查的当场查掉，把结果写进回复。
  只有查不了的（要用户操作、要用户决定、沙箱外的命令被拒）才留给用户，写明为什么查不了。
  一个办法失败了先想原因换个办法，至少试两种办法再说做不了。
- 用户点名了要看的文件时，先只看这些文件，够回答就回复；确实要核对别处（例如源码）才扩大，回复里说核对了哪些。
  不要为一个问题把整个仓库翻一遍：用户在等。
- 沙箱里跑 git、python3 时，开头会打印几行 xcrun、couldn't create cache file 的警告：沙箱不让写 /tmp，
  命令照常执行，看后面的输出和退出码就行，不用换办法重试。
- 写给用户的规则都在下面「最后一条消息」里，你不用读写作规范类的 skill 文件（例如 clear-writing-zh），把时间花在活上。

# 最后一条消息
最后一条消息是给用户的回复，用户直接看到。第一句直接回答用户问的，或说结论。
说一件活的状态时只用这些词：已完成、部分完成、未开始、进行中、已验证、未验证、失败、跳过、阻塞、未确认；
回答问题、介绍情况时不说状态。第一句不用状态词开头，例如不写「进行中，我读一下」「已完成，」。
不写工具名、JSON 和内部字段名。agent、skill、morula 是界面上的叫法，照原样写，不译成中文，也不加括号注释：
写「skill grill-me」，不写「技能（grill-me）」；写「agent」，不写「智能体（agent）」。
这几条高于其他说明（包括 AGENTS.md 这类全局规则），处理过程中说的话也照这样写。`

// agentToolNames 是挂给 agent 方式助理的工具。
var agentToolNames = []string{"list_projects", "list_agents", "list_harnesses", "read_project", "ask_user",
	"create_project", "rename_project", "merge_projects", "undo_merge", "move_runs", "dispatch", "transfer", "propose_team_change", "update_project", "run_command", "update_memory", "search_chats",
	"list_routines", "save_routine", "remove_routine", "delete_project"}

// agentMode 报告助理这次用不用 agent 方式。设置项 assistant.mode=pipeline 时退回两步。
func (e *Engine) agentMode(a *store.Agent) bool {
	return harness.Agentic(a.Runtime) && e.DB.Setting("assistant.mode", "") != "pipeline"
}

// takeThread 占用助理的会话，返回要续跑的会话 id（空表示新开）。会话正被占着时 fork 为 true：
// 这一路新开一个一次性的会话，不续跑，也不记会话 id。gen 交回 releaseThread。
// 一天一个会话：只在换日后新开。
// 助理的规则改过时照旧续跑，stale 为 true，由 agentPrompt 把新规则带上。
func (e *Engine) takeThread() (id string, fork bool, gen int, stale bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.threadBusy {
		return "", true, e.threadGen, false
	}
	e.threadBusy = true
	e.threadDay = journal.Today()
	v := strings.Split(e.DB.Setting("assistant.thread", ""), "|")
	switch {
	case len(v) == 3 && v[0] == journal.Today():
		return v[2], false, e.threadGen, v[1] != roleVer()
	case len(v) == 2 && v[0] == journal.Today(): // 升级前的格式没记规则版本
		return v[1], false, e.threadGen, true
	}
	return "", false, e.threadGen, false
}

// roleVer 是助理规则的版本。旧会话里存的是旧规则，续跑时要把新规则补发一遍。
func roleVer() string {
	h := fnv.New32a()
	h.Write([]byte(agentRole))
	return strconv.FormatUint(uint64(h.Sum32()), 36)
}

// saveThread 记下会话 id，同时记进当天的会话列表（assistant.thread.<日期>，盘点和翻旧会话用）。
// 期间清空过对话页或撤回过消息（gen 变了）时不记，下次新开。
func (e *Engine) saveThread(gen int, id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if gen != e.threadGen || id == "" {
		return
	}
	day := e.threadDay // 不用现在的日期：这轮跨过换日时，会话属于开始那天，换日后要新开
	if day == "" {
		day = journal.Today()
	}
	e.DB.SetSetting("assistant.thread", day+"|"+roleVer()+"|"+id)
	if h := strings.SplitN(e.DB.Setting("assistant.handoff", ""), "|", 3); len(h) == 3 && h[1] != id {
		e.DB.SetSetting("assistant.handoff", "") // 新会话已经带着交接记录开起来了
	}
	ids := dayThreads(e.DB, day)
	if len(ids) == 0 || ids[len(ids)-1] != id {
		e.DB.SetSetting("assistant.thread."+day, strings.Join(append(ids, id), " "))
	}
}

// dayThreads 是助理在 day 这天用过的会话，按开的先后。清空对话页后新开的会话排在后面。
func dayThreads(db *store.Store, day string) []string {
	return strings.Fields(db.Setting("assistant.thread."+day, ""))
}

func (e *Engine) releaseThread() {
	e.mu.Lock()
	e.threadBusy = false
	e.mu.Unlock()
}

// AssistantContext 是助理当前会话最后一次请求带的上下文 token 数（见 harness.Result.ContextTokens）。
// 还没有会话、或记下的数不是当前会话的时返回 0。
func (e *Engine) AssistantContext() int {
	v := strings.Split(e.DB.Setting("assistant.thread", ""), "|")
	c := strings.Split(e.DB.Setting("assistant.context", ""), "|")
	if len(v) != 3 || v[0] != journal.Today() || len(c) != 2 || c[0] != v[2] {
		return 0
	}
	n, _ := strconv.Atoi(c[1])
	return n
}

// CompactLimit 是助理会话到多少 token 时压缩：harness 会压缩的由它压缩，不会的由 morula 写交接记录换会话（compactAssistant）。
func (e *Engine) CompactLimit() int { return e.compactLimit() }

// ResetThread 在清空对话页、撤回消息后调用：旧会话里还留着被清掉的内容，下一条消息新开一个会话。
func (e *Engine) ResetThread() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.threadGen++
	e.missed = nil
	e.DB.SetSetting("assistant.thread", "")
	e.DB.SetSetting("assistant.handoff", "")
}

// noteMissed 记下会话外发生的事（并行的一路问答、任务树的汇报），下次续跑时告诉助理。
// prevProject 是上一条回复标的项目；它被并掉或不在了时返回 nil。
func (e *Engine) prevProject() *store.Project {
	e.mu.Lock()
	id := e.lastProj
	e.mu.Unlock()
	if p, _ := e.DB.Project(id); p != nil && p.MergedInto == 0 {
		return p
	}
	return nil
}

func (e *Engine) noteMissed(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.missed = append(e.missed, strings.TrimSpace(s))
	if len(e.missed) > 20 {
		e.missed = e.missed[len(e.missed)-20:]
	}
}

func (e *Engine) takeMissed() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := strings.Join(e.missed, "\n\n")
	e.missed = nil
	return s
}

func newMessage(text string, files []chat.File) string {
	return "# 用户的新消息\n" + text + attachNote(files)
}

// snapshot 是附在每条消息后面的团队名单和项目列表，问团队、问项目这类消息不用再调工具。
func (e *Engine) snapshot() string {
	e.DefaultProject() // 第一次用到时建好，列表里才有
	return "\n\n# 现在的团队（morula 附上的，和 list_agents 一样）\n" + clip(AgentList(e.DB), 4000) +
		"\n\n# 本机的 harness 和模型（morula 附上的，和 list_harnesses 一样）\n" + clip(HarnessList(e.DB, false), 6000) +
		"\n\n# 现在的项目（morula 附上的，和 list_projects 一样）\n" + clip(ProjectList(e.DB), 4000)
}

// agentPrompt 是这次运行的提示词。续跑时只带新消息和会话外发生的事，规则改过（stale）时把新规则放在前面；
// 新开时带上职责、全局记忆、最近几天的会话文件和今天最近的对话。
func (e *Engine) agentPrompt(a *store.Agent, resume, stale bool, msg string) string {
	missed := e.takeMissed()
	msg += e.snapshot()
	if resume {
		if missed != "" {
			msg = "# 你上次回复之后，对话里另外发生的事\n" + missed + "\n\n" + msg
		}
		if stale {
			msg = "# 你的规则更新了（替换这个会话里之前的规则，以这里为准）\n" + rules(a) + "\n\n" + msg
		}
		return msg
	}
	s := rules(a) + "\n\n# 记忆（morula 存着的，会话换了也还在）\n" + journal.Context(journal.Today(), 3)
	if h := e.handoff(); h != "" {
		s += "\n\n# 上一个会话的交接记录（今天早些时候的会话太长，换成了这个新会话；上一个会话的文件在下面的列表里）\n" + h
	}
	if f := e.sessionFiles(a); f != "" {
		s += "\n## 之前的会话文件（要查当时的原话、细节时用 search_chats 或直接读）\n\n" +
			"引用用户的原话时，从 morula 的对话记录里取：" + journal.DaysDir() + "/<日期>.jsonl，type 为 user 的行，text 是原话。" +
			"会话文件里的原话在每条提示词的「# 用户的新消息」下面。记忆和盘点里的是转述，不能当原话。文件大时先用 grep 找。\n\n" + f
	}
	return s + "\n\n# 今天最近的对话\n" + recent() + "\n\n" + msg
}

func rules(a *store.Agent) string {
	s := agentRole
	if strings.TrimSpace(a.Instructions) != "" {
		s += "\n\n# 你的职责说明\n" + a.Instructions
	}
	return s
}

// sessionFiles 列出今天和前 3 天助理用过的会话文件，一行一个。
func (e *Engine) sessionFiles(a *store.Agent) string {
	var b strings.Builder
	t, _ := time.ParseInLocation("2006-01-02", journal.Today(), time.Local)
	for i := 0; i <= 3; i++ {
		day := t.AddDate(0, 0, -i).Format("2006-01-02")
		for _, id := range dayThreads(e.DB, day) {
			if p := harness.SessionFile(a.Runtime, id); p != "" {
				b.WriteString("- " + day + "：" + p + "\n")
			}
		}
	}
	return b.String()
}

// agentTurn 用 agent 方式处理一条用户消息。
func (e *Engine) agentTurn(ctx context.Context, a *store.Agent, text string, files []chat.File) {
	title := text
	if strings.TrimSpace(title) == "" && len(files) > 0 {
		title = "发来附件 " + files[0].Name
	}
	f := e.newFlow(ctx, title)
	step := e.addStep(f, a.Name, "处理", "u")
	r, msg := &agentRun{e: e, a: a, text: text}, newMessage(text, files)
	if p := e.pinned(ctx); p != nil {
		r.project = p.ID
		e.setFlowProject(f, p)
		msg = pinnedNote(p) + "\n\n" + msg
	}
	e.agentLoop(withFlow(ctx, f, step), a, r, msg, files)
}

// pinned 是用户在哪个项目的页面里发的这条消息；在助理页发的、或项目已经没了时返回 nil。
func (e *Engine) pinned(ctx context.Context) *store.Project {
	id := chat.ProjectOf(ctx)
	if id == 0 {
		return nil
	}
	p, _ := e.DB.Project(id)
	return p
}

// pinnedNote 告诉助理这条消息已经定了项目：用户在项目页里发的，只在这个项目里做事。
func pinnedNote(p *store.Project) string {
	return fmt.Sprintf("# 这条消息在哪个项目里\n用户是在项目「%s」（编号 %d）的页面里发的，这条消息属于这个项目，不用判断归属，也不要新建项目。"+
		"派活、更新项目记忆、查安排都用这个项目；用户明确说要换到别的项目时再换。"+
		"要看这个项目的安排和记忆，调 read_project（编号 %d），查完再回答，不要只回「我查一下」就结束。", p.Name, p.ID, p.ID)
}

// agentLoop 跑助理的一次处理，直到给出回复、失败或被停止。msg 是这次要处理的事；用户补充时停掉重来。
// ctx 带着流程和「处理」这一步。
func (e *Engine) agentLoop(ctx context.Context, a *store.Agent, r *agentRun, msg string, files []chat.File) {
	fp := posOf(ctx)
	f, step := fp.f, fp.tip
	r.ctx = ctx
	state := "failed"
	defer func() {
		e.setStep(f, step, map[bool]string{true: "done", false: "failed"}[state == "done"])
		if f.Run == 0 { // 派了活的流程由 execRun 收尾
			e.endFlow(f, state)
		}
	}()

	tid, fork, gen, stale := e.takeThread()
	handoff := false // 要写交接记录时，会话由 compactAssistant 写完后放开
	defer func() {
		if !fork && !handoff {
			e.releaseThread()
		}
	}()
	x := e.newActivity(ctx, "", nil)
	prompt := e.agentPrompt(a, tid != "", stale, msg)
	images := imagesOf(files)
	mix := e.mixFor(tid)
	mix.addPrompt(prompt, rules(a), msg, tid != "", stale)
	for {
		url, release, err := e.tools.add(r.server())
		if err != nil {
			x.end("failed", false, 0)
			e.fail(ctx, "处理消息", err)
			return
		}
		s := spec(a, scratch(), prompt, "")
		s.Access, s.Resume, s.Ephemeral, s.Images = harness.ReadOnly, tid, fork, images
		s.Tools = &harness.Tools{URL: url, Names: agentToolNames, Timeout: 3600}
		if !fork && harness.Compacts(a.Runtime) {
			s.CompactAt = e.compactLimit() // 一天一个会话，太长时 harness 自己压缩
		}
		var res harness.Result
		more, err := interruptible(ctx, func(c context.Context) error {
			var err error
			res, err = harness.Ask(c, a.Runtime, s, nil, func(ev harness.Event) { mix.addEvent(ev); x.on(ev) })
			return err
		})
		release()
		if !fork && res.Session != "" {
			tid = res.Session
			e.saveThread(gen, tid)
			e.DB.SetSetting("assistant.context", tid+"|"+strconv.Itoa(res.ContextTokens()))
			e.saveMix(mix, tid)
		}
		if more != "" { // 用户补充了要求：停掉这次运行，带着补充重来
			r.text += "\n\n用户补充：" + more
			msg += "\n\n用户补充：" + more
			if tid != "" {
				prompt = "刚才那次处理被用户的补充打断了，按补充后的要求重新处理。\n\n" + msg
				images = nil
			} else {
				prompt = e.agentPrompt(a, false, false, msg)
			}
			mix.addPrompt(prompt, rules(a), msg, tid != "", false)
			continue
		}
		if err != nil {
			state = map[bool]string{true: "stopped", false: "failed"}[ctx.Err() != nil]
			x.end(state, false, res.Tokens)
			e.fail(ctx, "处理消息", err)
			return
		}
		x.end("done", true, res.Tokens)
		if fork {
			e.noteMissed("（另一路处理）" + oneLine(clip(msg, 300)) + "\n助理：" + res.Final)
		}
		extra := chat.Event{"activity": x.group}
		pid, run := r.where()
		if pid == 0 { // 这一轮没用到项目：多半是接着上一句说，沿用上一条回复的项目；没有时算默认项目
			p := e.prevProject()
			if p == nil {
				p = e.DefaultProject()
			}
			if p != nil {
				pid, run = p.ID, 0
				e.setFlowProject(f, p)
			}
		}
		e.mu.Lock()
		e.lastProj = pid
		e.mu.Unlock()
		if pid != 0 {
			extra["project"] = pid
			if run != 0 {
				extra["run"] = run
			}
		}
		e.say(ctx, "收到回复", res.Final, extra)
		state = "done"
		if !fork && tid != "" && !harness.Compacts(a.Runtime) && res.ContextTokens() > e.compactLimit() {
			handoff = true
			go func() {
				defer e.releaseThread()
				e.compactAssistant(a, gen, tid)
			}()
		}
		return
	}
}

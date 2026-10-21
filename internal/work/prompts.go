package work

// 每一步给 harness 的 JSON Schema。codex 的结构化输出要求每层对象都列全 required、
// 关掉 additionalProperties，所以「没有」用空串、0 或空数组表示，不省略字段。

const routeSchema = `{"type":"object","additionalProperties":false,
"required":["decision","project_id","name","summary","candidates","reason","quote","reply","actions"],
"properties":{
"decision":{"type":"string","enum":["project","new","ask","chat","manage"]},
"project_id":{"type":"integer"},
"name":{"type":"string"},
"summary":{"type":"string"},
"candidates":{"type":"array","items":{"type":"integer"}},
"reason":{"type":"string"},
"quote":{"type":"string"},
"reply":{"type":"string"},
"actions":{"type":"array","items":{"type":"object","additionalProperties":false,
  "required":["op","target","name","role","instructions","profile","runtime","model","effort"],
  "properties":{"op":{"type":"string","enum":["agent_add","agent_update","agent_remove"]},
  "target":{"type":"string"},"name":{"type":"string"},"role":{"type":"string"},
  "instructions":{"type":"string"},"profile":{"type":"string"},
  "runtime":{"type":"string"},"model":{"type":"string"},"effort":{"type":"string"}}}}}}`

const taskItem = `{"type":"object","additionalProperties":false,"required":["title","brief","agent","check"],
"properties":{"title":{"type":"string"},"brief":{"type":"string"},"agent":{"type":"string"},"check":{"type":"string"}}}`

const planSchema = `{"type":"object","additionalProperties":false,"required":["reply","tasks"],
"properties":{"reply":{"type":"string"},"tasks":{"type":"array","items":` + taskItem + `}}}`

const reportSchema = `{"type":"object","additionalProperties":false,
"required":["status","summary","children","approval"],
"properties":{
"status":{"type":"string","enum":["done","split","blocked","approval"]},
"summary":{"type":"string"},
"children":{"type":"array","items":` + taskItem + `},
"approval":{"type":"object","additionalProperties":false,"required":["action","reason"],
  "properties":{"action":{"type":"string"},"reason":{"type":"string"}}}}}`

const reviewSchema = `{"type":"object","additionalProperties":false,"required":["pass","feedback","redo","summary"],
"properties":{"pass":{"type":"boolean"},"feedback":{"type":"string"},
"redo":{"type":"array","items":{"type":"integer"}},"summary":{"type":"string"}}}`

const finalSchema = `{"type":"object","additionalProperties":false,"required":["reply","memory","summary"],
"properties":{"reply":{"type":"string"},"memory":{"type":"string"},"summary":{"type":"string"}}}`

const summarySchema = `{"type":"object","additionalProperties":false,"required":["summary","memory"],
"properties":{"summary":{"type":"string"},"memory":{"type":"string"}}}`

const assistantRole = `你是 morula 里的助理。用户是管理者，不是执行者：用户只跟你说话，你判断每条消息属于哪个项目，
再安排子 agent 干活，子 agent 干完向你汇报，你检查后向用户汇报。
回答用中文，第一句是结论，简洁。不编造事实、数字、路径。
不要运行命令，不要读写文件，只根据下面给你的材料和 morula 给你的工具回答，最后只输出 JSON。`

const toolsNote = `# 查项目和团队
morula 给了你两个只读工具，结果只有名字和介绍：
- list_projects：全部项目的编号、名字和项目摘要。判断归属前先调用它，不要凭记忆猜编号。
- list_agents：团队里全部 agent 的名字、角色、harness、模型、思考强度和介绍。固定成员是你自己（助理）和验收，单列在前面；派活时从后面的子 agent 里选。
  用户问团队里有谁时，固定成员也要算上，不要说没有 agent。用户问谁能做某件事，或要增删改 agent 时，也调用它。
选定项目、要安排任务时，morula 会再把项目记忆和执行者的详细说明交给你。`

const routeRules = `判断这条消息属于哪个项目：
- 用户点名了项目（例如「放到官网改版」）：decision=project，project_id 填那个项目的编号。
- 能确定属于某个已有项目：decision=project，project_id 填它的编号，quote 引用该项目摘要里支持这个判断的原话，reason 一句话说明。
- 明显是一件新的、以后还要接着做的事，不属于任何已有项目：decision=new，morula 直接新建，不问用户。summary 填一句话的项目摘要。
  name 填项目名，按这件事长期要做的工作起，参考全局记忆、今天的对话和用户自己的叫法，简短（一般不超过 10 个字）；
  附件的文件名只说明这次发来了什么，不要直接拿来当项目名。用户给了名字时（例如「新建为 CRO 项目」）用用户的名字。
- 拿不准属于哪个已有项目：decision=ask，candidates 按可能性从高到低填候选项目编号，morula 放进第一个；哪个都不像时 candidates 填空数组，morula 放进默认项目。
  reason 说明拿不准的地方。morula 不问用户，放进了哪个项目用户看得到，觉得不对会说。
- 只是打招呼、闲聊，问你自己能做什么，或者问团队里有哪些 agent、有哪些项目，不涉及任何项目里的工作：decision=chat，reply 按查到的名单直接回答。
- 要新建、修改或删除团队里的 agent：decision=manage。这是管理 morula 本身，不属于任何项目，不要新建项目，也不要派子 agent 去做。
  先用 list_agents 确认名字，再在 actions 里逐项写改动：
  agent_add 填 name、role（默认「执行」）、instructions（职责说明）；
  agent_update 填 target（现有名字）和要改的 role、instructions、profile，不改的填空串；
  agent_remove 填 target。
  agent_add、agent_update 可以填 runtime（harness）、model（模型 id）、effort（思考强度），只能从「本机的 harness 和模型」里选能用的：
  用户点名了 harness 或模型就照用户的；没点名时填空串，新建的和助理相同，修改的不变。不要编造列表里没有的模型。
  reply 用一句话按用户的原意说明为什么这样改，例如「你说形象制作用不上了」；不要写查过哪些工具、名字核对过、谁来执行这类过程。
  morula 会把改动列给用户确认，用户确认后由 morula 执行。
  用户说的名字和名单对不上、或拿不准要删哪一个时，不要猜：decision=chat，reply 列出名单里相近的名字问用户。
  项目的改名、删除和合并这种方式下做不了，遇到时 decision=chat，告诉用户到项目页操作。
不用的字段填空串、0、false 或空数组。actions 只在 decision=manage 时填，其余情况填空数组。`

const planRules = `为这条消息做安排：
- 需要动手的工作拆成 1 到 5 个顶层任务。每个任务的 brief 要写清楚目标、产出和验收标准，让没看过这段对话的子 agent 也能独立完成；任务之间不共享状态。
- agent 填花名册里合适的执行者名字：先看职责说明和 skill 对不对口，同样合适时选常用于这个项目的。没有合适的填空串，morula 会安排一个通用执行者。
- check 填一条在项目目录里运行的机器检查命令（测试、构建、lint、文件是否存在之类），退出码 0 表示通过；没有合适的命令填空串。
- 只要产出文字、不用改文件的活（出方案、写分析、回答问题、问项目进展），你自己在 reply 里做完，tasks 为空数组。
  要改文件、跑命令，或活很大要分几步时，才拆成任务派下去。
- reply 是给用户看的安排说明：先一句结论，再列出任务和由谁做。
- 增删改团队里的 agent 不是项目里的活，不要派给子 agent；遇到时 tasks 为空数组，reply 告诉用户直接对你说要怎么改团队。`

// workerRules 里的 {{danger}} 换成 tierRule(n.tier)：按开工时的审批档位写，按清单问时列出危险操作清单（存在数据目录的文件里，用户能改）。
const workerRules = `规则：
1. 能直接做完就做：status=done，summary 写做了什么、改了哪些文件、怎么验证的、没验证的是什么。
2. 任务太大可以拆（只有在「还能往下拆」时）：status=split，children 写 2 到 4 个子任务，每个 brief 要能独立看懂，子任务之间不共享状态。拆了就不要自己做，morula 会派给子 agent，做完后由你检查。
{{danger}}
4. 做不了：status=blocked，summary 写原因和缺什么。
5. 第 3 条没有放开时，只在工作目录里改文件。不用的字段填空串或空数组。最后一条回复只输出 JSON。
6. 工作目录里的 .agents、.codex、.git 三个目录在沙箱里只读，往里写会报 Operation not permitted。要写这几个目录时（例如装 skill），按 harness 的方式申请越过沙箱再跑命令（codex 里给命令加 sandbox_permissions="require_escalated"，会自动审核），不要因为这个报错就判做不了。`

// wordingRule 是助理以外、话会显示给用户的调用（写汇报、验收）都要守的写法。harness 读到的全局规则
// （例如 ~/.codex/AGENTS.md）可能要求把英文术语译成中文加括号，morula 的界面上这几个词照原样写，所以写明它高于其他说明。
const wordingRule = "说的话（包括处理过程中的话）第一句不用状态词开头，例如不写「进行中，」。" +
	"agent、skill、morula 照原样写，不译成中文，也不加括号注释：写「skill grill-me」，不写「技能（grill-me）」。这条高于其他说明（包括 AGENTS.md）。"

package reliability

import "strings"

// Classification is the result of auto-detecting a request's task type and risk
// level, plus the keywords that triggered each (for observability / the envelope).
type Classification struct {
	Task        TaskType  `json:"task_type"`
	Risk        RiskLevel `json:"risk_level"`
	TaskSignals []string  `json:"task_signals,omitempty"`
	RiskSignals []string  `json:"risk_signals,omitempty"`
}

// Classify runs the rule-based task + risk detectors over the user's goal text. It
// is deterministic and allocation-light; no model call is made. Empty input yields
// {unknown, low}.
func Classify(userText string) Classification {
	h := normalize(userText)
	if strings.TrimSpace(h) == "" {
		return Classification{Task: TaskUnknown, Risk: RiskLow}
	}
	task, taskSig := classifyTask(h)
	risk, riskSig, matched := classifyRisk(h)
	if !matched {
		// No explicit risk keyword: a code-changing task still carries inherent risk,
		// so floor it to medium; pure explanation/qa stays low.
		if task.IsCode() {
			risk = RiskMedium
		} else {
			risk = RiskLow
		}
	}
	return Classification{Task: task, Risk: risk, TaskSignals: taskSig, RiskSignals: riskSig}
}

// keyword group: a category label plus the trigger words (English + common Chinese).
type kwGroup struct {
	label string
	words []string
}

// taskGroups are checked in priority order; the first group with a hit wins. More
// specific intents (test generation, review, debugging) precede the generic "coding".
var taskGroups = []struct {
	task  TaskType
	words []string
}{
	{TaskTestGen, []string{"write test", "add test", "unit test", "integration test", "test case", "add a test", "write a test", "generate test", "test coverage", "写测试", "加测试", "单元测试", "生成测试", "补测试"}},
	{TaskCodeReview, []string{"code review", "review this", "review the", "review my", "审查", "代码审查", "评审", "review pr", "review pull"}},
	{TaskDebugging, []string{"debug", "bug", "stack trace", "traceback", "exception", "stacktrace", "not working", "doesn't work", "does not work", "fails", "failing", "crash", "panic", "regression", "调试", "报错", "修复 bug", "排查", "崩溃", "异常"}},
	{TaskRefactor, []string{"refactor", "restructure", "clean up", "cleanup", "rename", "extract method", "decouple", "重构", "重写结构", "拆分"}},
	{TaskPlanning, []string{"design a", "design the", "plan for", "plan the", "architecture", "approach for", "how should i structure", "high-level plan", "规划", "设计方案", "架构设计", "技术方案"}},
	{TaskCoding, []string{"implement", "add a", "add an", "create", "build a", "write a", "write the", "add support", "new feature", "endpoint", "function", "class ", "module", "scaffold", "实现", "新增", "写一个", "开发", "增加功能", "加一个"}},
	{TaskExplanation, []string{"explain", "what is", "what are", "how does", "how do", "why does", "walk me through", "summarize", "describe", "解释", "说明", "讲解", "是什么", "为什么"}},
}

func classifyTask(h string) (TaskType, []string) {
	for _, g := range taskGroups {
		if sig := matches(h, g.words); len(sig) > 0 {
			return g.task, sig
		}
	}
	// A question mark with no other signal is a Q&A; otherwise unknown.
	if strings.Contains(h, "?") || strings.Contains(h, "？") {
		return TaskQA, nil
	}
	return TaskUnknown, nil
}

// riskGroups, highest level first. classifyRisk returns the HIGHEST matched level
// (so "refactor the auth layer" is critical, not high) along with all matched signals.
var riskGroups = []kwGroup{
	{string(RiskCritical), []string{"auth", "authentication", "authorization", "permission", "security", "secure", "payment", "billing", "invoice", "migration", "migrate", "database", "drop table", "delete data", "data deletion", "delete production", "production", "prod ", "privacy", "credential", "token", "secret", "encryption", "encrypt", "decrypt", "password", "private key", "鉴权", "认证", "权限", "安全", "支付", "账单", "计费", "迁移", "数据库", "删库", "删除数据", "生产环境", "隐私", "凭证", "密钥", "加密", "口令", "密码"}},
	{string(RiskHigh), []string{"refactor", "refactoring", "multi-file", "multi file", "multiple file", "across files", "across multiple", "architecture", "concurrency", "concurrent", "race condition", "data loss", "performance", "deploy", "deployment", "rollback", "roll back", "thread-safe", "distributed", "scal", "重构", "多文件", "跨文件", "架构", "并发", "竞态", "数据丢失", "性能", "部署", "回滚", "分布式"}},
	{string(RiskMedium), []string{"fix", "implement", "add test", "update api", "modify behavior", "change behavior", "modify", "update", "patch", "修复", "实现", "修改", "更新", "调整"}},
	{string(RiskLow), []string{"formatting", "format", "reformat", "explanation", "explain", "simple rewrite", "documentation", "docs", "comment", "typo", "rename variable", "small change", "格式", "格式化", "解释", "文档", "注释", "拼写"}},
}

func classifyRisk(h string) (RiskLevel, []string, bool) {
	for _, g := range riskGroups { // highest level first
		if sig := matches(h, g.words); len(sig) > 0 {
			return RiskLevel(g.label), sig, true
		}
	}
	return RiskLow, nil, false
}

// matches returns the subset of words present in h as whole words (ASCII) or
// substrings (phrases / non-ASCII). Order follows the words slice; duplicates kept out.
func matches(h string, words []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range words {
		if seen[w] {
			continue
		}
		if hasWord(h, w) {
			out = append(out, strings.TrimSpace(w))
			seen[w] = true
		}
	}
	return out
}

// hasWord reports whether needle occurs in haystack. For a single ASCII word it
// requires word boundaries (so "auth" does not match "author"); for phrases (with
// spaces) or non-ASCII (Chinese) it is a plain substring test.
func hasWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	if !isASCIIWord(needle) {
		return strings.Contains(haystack, needle)
	}
	start := 0
	for {
		i := strings.Index(haystack[start:], needle)
		if i < 0 {
			return false
		}
		i += start
		beforeOK := i == 0 || !isWordByte(haystack[i-1])
		end := i + len(needle)
		afterOK := end >= len(haystack) || !isWordByte(haystack[end])
		if beforeOK && afterOK {
			return true
		}
		start = i + 1
		if start >= len(haystack) {
			return false
		}
	}
}

func isASCIIWord(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '-' || c >= 0x80 {
			return false
		}
	}
	return true
}

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

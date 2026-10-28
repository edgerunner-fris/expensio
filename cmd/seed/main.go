// seed 通过公开 HTTP API 生成一份可重复的演示数据：租户、审批人、预算、
// 四种状态的费用单、订阅与一张待支付账单。
//
// 为什么走 API 而不是直连数据库：种子数据必须经过真实的鉴权、RLS、
// 状态机与预算预占，否则"演示环境能跑"证明不了任何东西。也正因为如此，
// 它需要先 `make app-up`。
//
// 幂等：预算按名称 + 期间匹配，费用单按描述里的 seed 标记匹配，
// 审批人按成员列表匹配。重复执行只会补齐缺失的部分，不会重复造数。
// 账单不重复开具：已有 open 账单时直接复用。
//
// 口令只从环境变量读取，不接受命令行参数（会进 shell 历史与 ps）：
//
//	EXPENSIO_SEED_PASSWORD  必填，10–128 字节，owner 与审批人共用
//
// 用法：
//
//	EXPENSIO_SEED_PASSWORD=... go run ./cmd/seed
//	EXPENSIO_SEED_PASSWORD=... go run ./cmd/seed -base-url http://localhost:8080
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	minPasswordLen = 10
	maxResponse    = 4 << 20
	apiPrefix      = "/api/v1"
)

func main() { os.Exit(run()) }

type config struct {
	baseURL       string
	tenantSlug    string
	tenantName    string
	email         string
	approverEmail string
	currency      string
	password      string
	timeout       time.Duration
}

func run() int {
	var cfg config
	flag.StringVar(&cfg.baseURL, "base-url", "http://localhost:8080", "网关地址")
	flag.StringVar(&cfg.tenantSlug, "tenant-slug", "demo-seed", "演示租户 slug")
	flag.StringVar(&cfg.tenantName, "tenant-name", "Demo Seed", "演示租户名称")
	flag.StringVar(&cfg.email, "email", "owner@demo-seed.test", "owner 邮箱")
	flag.StringVar(&cfg.approverEmail, "approver-email", "approver@demo-seed.test", "审批人邮箱")
	flag.StringVar(&cfg.currency, "currency", "USD", "演示币种")
	flag.DurationVar(&cfg.timeout, "timeout", 90*time.Second, "整体超时")
	flag.Parse()

	cfg.password = os.Getenv("EXPENSIO_SEED_PASSWORD")
	if len(cfg.password) < minPasswordLen {
		return fail("EXPENSIO_SEED_PASSWORD 必须设置且至少 %d 字节", minPasswordLen)
	}
	base, err := parseBaseURL(cfg.baseURL)
	if err != nil {
		return fail("%v", err)
	}
	cfg.baseURL = base
	cfg.currency = strings.ToUpper(strings.TrimSpace(cfg.currency))
	if len(cfg.currency) != 3 {
		return fail("-currency 必须是三位币种代码")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	s := &seeder{cli: &client{base: cfg.baseURL, http: &http.Client{Timeout: 20 * time.Second}}, cfg: cfg}
	summary, err := s.run(ctx)
	if err != nil {
		return fail("%v", err)
	}
	printSummary(s.cfg, summary)
	return 0
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "seed: "+format+"\n", args...)
	return 1
}

func parseBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("-base-url 必须是完整的 http(s) 地址，当前为 %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("-base-url 只支持 http/https，当前为 %q", u.Scheme)
	}
	if u.User != nil {
		return "", errors.New("-base-url 不能包含用户名或口令")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// ---- HTTP ----

type apiError struct {
	status    int
	code      string
	message   string
	requestID string
}

func (e *apiError) Error() string {
	msg := fmt.Sprintf("HTTP %d %s", e.status, e.code)
	if e.message != "" {
		msg += ": " + e.message
	}
	if e.requestID != "" {
		msg += " (request_id=" + e.requestID + ")"
	}
	return msg
}

func isStatus(err error, status int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == status
}

type client struct {
	base string
	http *http.Client
}

type envelope struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	RequestID string          `json:"request_id"`
	Data      json.RawMessage `json:"data"`
}

func (c *client) do(ctx context.Context, method, path, token string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+apiPrefix+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "expensio-seed/1.0")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &apiError{status: resp.StatusCode, code: env.Code, message: env.Message, requestID: env.RequestID}
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("%s %s: 响应 data 解析失败: %w", method, path, err)
		}
	}
	return nil
}

// ---- 领域视图（只声明种子需要的字段）----

type profile struct {
	UserID     string `json:"user_id"`
	Email      string `json:"email"`
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Role       string `json:"role"`
}

type session struct {
	AccessToken string  `json:"access_token"`
	Profile     profile `json:"profile"`
}

type member struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

type memberPage struct {
	Items []member `json:"items"`
	Total int64    `json:"total"`
}

type invitation struct {
	Link        string `json:"link"`
	EmailQueued bool   `json:"email_queued"`
}

type invitationPage struct {
	Items []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"items"`
}

type budget struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Currency    string `json:"currency"`
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
	AmountMinor int64  `json:"amount_minor"`
}

type budgetPage struct {
	Items []budget `json:"items"`
}

type expense struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Description string `json:"description"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

type expensePage struct {
	Items []expense `json:"items"`
}

type comment struct {
	Body string `json:"body"`
}

type commentList struct {
	Items []comment `json:"items"`
}

type plan struct {
	Code       string `json:"code"`
	PriceMinor int64  `json:"price_minor"`
	Currency   string `json:"currency"`
	Interval   string `json:"interval"`
}

type subscription struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type invoice struct {
	ID          string `json:"id"`
	Number      string `json:"number"`
	Status      string `json:"status"`
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	DueOn       string `json:"due_on"`
}

type invoicePage struct {
	Items []invoice `json:"items"`
}

// ---- 种子逻辑 ----

type seeder struct {
	cli *client
	cfg config

	owner      *session
	approver   *session
	budgets    map[string]budget // category -> 演示预算
	budgetNew  int
	expenseNew int
}

type summary struct {
	TenantSlug string
	Owner      string
	Approver   string
	Budgets    int
	BudgetNew  int
	Expenses   int
	ExpenseNew int
	Invoice    *invoice
}

func (s *seeder) run(ctx context.Context) (*summary, error) {
	var err error
	if s.owner, err = s.ensureSession(ctx, s.cfg.email, "owner", s.cfg.tenantName); err != nil {
		return nil, err
	}

	// 所有写操作都使用 owner 的会话；审批必须由另一个账号执行（禁止自审）。
	if err := s.ensureApprover(ctx); err != nil {
		return nil, err
	}
	if err := s.ensurePolicy(ctx); err != nil {
		return nil, err
	}
	if err := s.ensureBudgets(ctx); err != nil {
		return nil, err
	}
	if err := s.ensureExpenses(ctx); err != nil {
		return nil, err
	}
	inv, err := s.ensureBilling(ctx)
	if err != nil {
		return nil, err
	}
	return &summary{
		TenantSlug: s.cfg.tenantSlug,
		Owner:      s.cfg.email,
		Approver:   s.cfg.approverEmail,
		Budgets:    len(s.budgets),
		BudgetNew:  s.budgetNew,
		Expenses:   4,
		ExpenseNew: s.expenseNew,
		Invoice:    inv,
	}, nil
}

func (s *seeder) ensureSession(ctx context.Context, email, role, tenantName string) (*session, error) {
	var sess session
	err := s.cli.do(ctx, http.MethodPost, "/auth/login", "", map[string]any{
		"tenant_slug": s.cfg.tenantSlug, "email": email, "password": s.cfg.password,
	}, &sess)
	if err == nil {
		if role == "owner" && sess.Profile.Role != "owner" {
			return nil, fmt.Errorf("%s 在该租户的角色是 %s，不是 owner", email, sess.Profile.Role)
		}
		return &sess, nil
	}
	if !isStatus(err, http.StatusUnauthorized) {
		return nil, fmt.Errorf("登录 %s 失败: %w", email, err)
	}

	if role != "owner" {
		return nil, fmt.Errorf("%s 尚未加入租户 %s", email, s.cfg.tenantSlug)
	}
	// 首次运行：注册租户与 owner。租户已存在但密码不匹配时，register 返回 409，
	// 错误信息会提示账号已存在——这通常是 EXPENSIO_SEED_PASSWORD 与既有密码不一致。
	err = s.cli.do(ctx, http.MethodPost, "/auth/register", "", map[string]any{
		"tenant_slug":  s.cfg.tenantSlug,
		"tenant_name":  tenantName,
		"email":        email,
		"password":     s.cfg.password,
		"display_name": "Seed Owner",
	}, &sess)
	if err != nil {
		return nil, fmt.Errorf("注册演示租户失败: %w", err)
	}
	return &sess, nil
}

func (s *seeder) ensureApprover(ctx context.Context) error {
	var page memberPage
	if err := s.cli.do(ctx, http.MethodGet, "/tenants/current/members?page_size=100", s.owner.AccessToken, nil, &page); err != nil {
		return fmt.Errorf("读取成员列表失败: %w", err)
	}
	for _, m := range page.Items {
		if strings.EqualFold(m.Email, s.cfg.approverEmail) && m.Status == "active" {
			if m.Role != "admin" && m.Role != "owner" {
				// 既有成员可能被降过级；审批需要 admin，种子数据负责把演示环境修好。
				if err := s.cli.do(ctx, http.MethodPatch,
					"/tenants/current/members/"+m.UserID, s.owner.AccessToken,
					map[string]any{"role": "admin"}, nil); err != nil {
					return fmt.Errorf("提升审批人角色失败: %w", err)
				}
			}
			sess, err := s.ensureSession(ctx, s.cfg.approverEmail, "approver", "")
			if err != nil {
				return err
			}
			s.approver = sess
			return nil
		}
	}

	inv, err := s.createInvitation(ctx)
	if err != nil {
		return err
	}
	token, err := invitationToken(inv.Link)
	if err != nil {
		return err
	}
	var sess session
	err = s.cli.do(ctx, http.MethodPost, "/invitations/"+url.PathEscape(token)+"/accept", "", map[string]any{
		"password": s.cfg.password, "display_name": "Seed Approver",
	}, &sess)
	if err != nil {
		// 邮箱可能已经是全局账号（例如换了租户重新播种）：用同一口令再登录一次。
		var retry session
		if loginErr := s.cli.do(ctx, http.MethodPost, "/auth/login", "", map[string]any{
			"tenant_slug": s.cfg.tenantSlug, "email": s.cfg.approverEmail, "password": s.cfg.password,
		}, &retry); loginErr == nil {
			s.approver = &retry
			return nil
		}
		return fmt.Errorf("接受邀请失败: %w", err)
	}
	s.approver = &sess
	return nil
}

// createInvitation 创建审批人邀请。上一次运行可能留下待接受邀请（没有保存链接），
// 此时先撤销同邮箱的旧邀请再创建，避免种子数据被一次中断永久卡住。
func (s *seeder) createInvitation(ctx context.Context) (*invitation, error) {
	in := map[string]any{"email": s.cfg.approverEmail, "role": "admin"}
	var inv invitation
	err := s.cli.do(ctx, http.MethodPost, "/tenants/current/invitations", s.owner.AccessToken, in, &inv)
	if err == nil {
		return &inv, nil
	}
	if !isStatus(err, http.StatusConflict) {
		return nil, fmt.Errorf("创建审批人邀请失败: %w", err)
	}

	var page invitationPage
	if listErr := s.cli.do(ctx, http.MethodGet, "/tenants/current/invitations?page_size=100",
		s.owner.AccessToken, nil, &page); listErr != nil {
		return nil, fmt.Errorf("读取待接受邀请失败: %w", listErr)
	}
	for _, item := range page.Items {
		if !strings.EqualFold(item.Email, s.cfg.approverEmail) {
			continue
		}
		if delErr := s.cli.do(ctx, http.MethodDelete,
			"/tenants/current/invitations/"+item.ID, s.owner.AccessToken, nil, nil); delErr != nil {
			return nil, fmt.Errorf("撤销旧邀请失败: %w", delErr)
		}
	}
	if err := s.cli.do(ctx, http.MethodPost, "/tenants/current/invitations", s.owner.AccessToken, in, &inv); err != nil {
		return nil, fmt.Errorf("重新创建审批人邀请失败: %w", err)
	}
	return &inv, nil
}

func (s *seeder) ensurePolicy(ctx context.Context) error {
	// 门槛设得远高于演示金额：演示单走单级审批，审批流仍由 approver 真实执行。
	// 同时让"审批策略"这一页有数据。
	err := s.cli.do(ctx, http.MethodPut, "/approval-policies/"+s.cfg.currency, s.owner.AccessToken, map[string]any{
		"second_level_threshold_minor": int64(50_000_00),
	}, nil)
	if err != nil {
		return fmt.Errorf("设置审批策略失败: %w", err)
	}
	return nil
}

type budgetSpec struct {
	name     string
	category string
	amount   int64
}

func (s *seeder) ensureBudgets(ctx context.Context) error {
	first, last := monthRange(time.Now().UTC())
	specs := []budgetSpec{
		{"种子差旅预算", "travel", 5_000_00},
		{"种子餐饮预算", "meals", 1_000_00},
		{"种子办公预算", "office", 2_000_00},
		{"种子软件预算", "software", 3_000_00},
	}
	s.budgets = make(map[string]budget, len(specs))

	var page budgetPage
	if err := s.cli.do(ctx, http.MethodGet, "/budgets?page_size=100", s.owner.AccessToken, nil, &page); err != nil {
		return fmt.Errorf("读取预算失败: %w", err)
	}
	for _, spec := range specs {
		found := false
		for _, b := range page.Items {
			if b.Name == spec.name && b.Category == spec.category &&
				b.Currency == s.cfg.currency && b.PeriodStart == first && b.PeriodEnd == last {
				s.budgets[spec.category] = b
				found = true
				break
			}
		}
		if found {
			continue
		}
		var created budget
		err := s.cli.do(ctx, http.MethodPost, "/budgets", s.owner.AccessToken, map[string]any{
			"name":         spec.name,
			"category":     spec.category,
			"currency":     s.cfg.currency,
			"period_start": first,
			"period_end":   last,
			"amount_minor": spec.amount,
		}, &created)
		if err != nil {
			return fmt.Errorf("创建预算 %q 失败: %w", spec.name, err)
		}
		s.budgets[spec.category] = created
		s.budgetNew++
	}
	return nil
}

type expenseSpec struct {
	category    string
	merchant    string
	description string
	amount      int64
	target      string // draft / submitted / approved / rejected / reimbursed
}

func (s *seeder) ensureExpenses(ctx context.Context) error {
	// 描述同时是幂等键：重复执行按它匹配既有单据。
	specs := []expenseSpec{
		{"travel", "中国铁路", "seed:客户现场差旅", 128_00, "reimbursed"},
		{"meals", "Blue Bottle", "seed:团队午餐", 36_50, "approved"},
		{"office", "宜家", "seed:工位显示器支架", 79_00, "submitted"},
		{"software", "JetBrains", "seed:IDE 年费", 249_00, "rejected"},
	}

	var page expensePage
	if err := s.cli.do(ctx, http.MethodGet, "/expenses?scope=all&page_size=100", s.owner.AccessToken, nil, &page); err != nil {
		return fmt.Errorf("读取费用单失败: %w", err)
	}
	for _, spec := range specs {
		var cur *expense
		for i := range page.Items {
			if page.Items[i].Description == spec.description {
				cur = &page.Items[i]
				break
			}
		}
		if cur == nil {
			var created expense
			in := map[string]any{
				"category":     spec.category,
				"currency":     s.cfg.currency,
				"amount_minor": spec.amount,
				"merchant":     spec.merchant,
				"description":  spec.description,
				"occurred_at":  time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339),
			}
			if b, ok := s.budgets[spec.category]; ok {
				in["budget_id"] = b.ID
			}
			if err := s.cli.do(ctx, http.MethodPost, "/expenses", s.owner.AccessToken, in, &created); err != nil {
				return fmt.Errorf("创建费用单 %q 失败: %w", spec.description, err)
			}
			cur = &created
			s.expenseNew++
		}
		if err := s.driveExpense(ctx, cur, spec); err != nil {
			return fmt.Errorf("推进费用单 %q 失败: %w", spec.description, err)
		}
	}
	// 在软件费用上留一条评论，便于演示协作区（重复执行不会重复追加）。
	if err := s.ensureComment(ctx); err != nil {
		return err
	}
	return nil
}

// driveExpense 把单据从当前状态推进到目标状态。每次动作后只看返回值，
// 不假设动作一定生效：服务端状态机才是唯一事实来源。
func (s *seeder) driveExpense(ctx context.Context, cur *expense, spec expenseSpec) error {
	steps := 0
	for cur.Status != spec.target && steps < 4 {
		steps++
		var err error
		switch {
		case cur.Status == "draft" && spec.target != "draft":
			err = s.action(ctx, s.owner, cur.ID, "submit", "种子数据：提交审批")
		case cur.Status == "submitted" && (spec.target == "approved" || spec.target == "reimbursed"):
			err = s.action(ctx, s.approver, cur.ID, "approve", "种子数据：审批通过")
		case cur.Status == "submitted" && spec.target == "rejected":
			err = s.action(ctx, s.approver, cur.ID, "reject", "种子数据：缺少发票")
		case cur.Status == "approved" && spec.target == "reimbursed":
			err = s.action(ctx, s.owner, cur.ID, "reimburse", "种子数据：已报销")
		default:
			return fmt.Errorf("状态 %s 无法推进到 %s", cur.Status, spec.target)
		}
		if err != nil {
			return err
		}
		if err := s.cli.do(ctx, http.MethodGet, "/expenses/"+cur.ID, s.owner.AccessToken, nil, cur); err != nil {
			return err
		}
	}
	if cur.Status != spec.target {
		return fmt.Errorf("最终状态是 %s，期望 %s", cur.Status, spec.target)
	}
	return nil
}

func (s *seeder) action(ctx context.Context, who *session, expenseID, action, comment string) error {
	return s.cli.do(ctx, http.MethodPost, "/expenses/"+expenseID+"/"+action, who.AccessToken,
		map[string]any{"comment": comment}, nil)
}

func (s *seeder) ensureComment(ctx context.Context) error {
	body := "种子数据：这张单缺少发票，请补充后重新提交。"
	var page expensePage
	if err := s.cli.do(ctx, http.MethodGet, "/expenses?scope=all&page_size=100", s.owner.AccessToken, nil, &page); err != nil {
		return err
	}
	for _, e := range page.Items {
		if e.Description != "seed:IDE 年费" {
			continue
		}
		var list commentList
		if err := s.cli.do(ctx, http.MethodGet, "/expenses/"+e.ID+"/comments", s.owner.AccessToken, nil, &list); err != nil {
			return err
		}
		for _, c := range list.Items {
			if c.Body == body {
				return nil
			}
		}
		return s.cli.do(ctx, http.MethodPost, "/expenses/"+e.ID+"/comments", s.approver.AccessToken,
			map[string]any{"body": body}, nil)
	}
	return nil
}

func (s *seeder) ensureBilling(ctx context.Context) (*invoice, error) {
	var plans []plan
	if err := s.cli.do(ctx, http.MethodGet, "/billing/plans", s.owner.AccessToken, nil, &plans); err != nil {
		return nil, fmt.Errorf("读取套餐失败: %w", err)
	}
	if len(plans) == 0 {
		return nil, errors.New("套餐目录为空，请先执行迁移")
	}
	chosen := plans[0]
	for _, p := range plans {
		if p.Interval == "monthly" {
			chosen = p
			break
		}
	}

	var sub subscription
	err := s.cli.do(ctx, http.MethodGet, "/billing/subscription", s.owner.AccessToken, nil, &sub)
	// 没有订阅，或当前订阅已取消时，开通一条新的。
	if isStatus(err, http.StatusNotFound) || (err == nil && sub.Status == "canceled") {
		err = s.cli.do(ctx, http.MethodPost, "/billing/subscriptions", s.owner.AccessToken, map[string]any{
			"plan_code": chosen.Code, "seats": 5,
		}, &sub)
		if err != nil {
			return nil, fmt.Errorf("开通订阅失败: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("读取当前订阅失败: %w", err)
	}

	var inv invoice
	err = s.cli.do(ctx, http.MethodPost, "/billing/subscriptions/"+sub.ID+"/invoices", s.owner.AccessToken, nil, &inv)
	if err == nil {
		return &inv, nil
	}
	if !isStatus(err, http.StatusConflict) {
		return nil, fmt.Errorf("开具账单失败: %w", err)
	}
	// 本周期已经出过账：复用最近一张未支付账单。
	var page invoicePage
	if listErr := s.cli.do(ctx, http.MethodGet, "/billing/invoices?status=open&page_size=20", s.owner.AccessToken, nil, &page); listErr != nil {
		return nil, fmt.Errorf("读取账单失败: %w", listErr)
	}
	if len(page.Items) == 0 {
		return nil, errors.New("本周期账单已存在但找不到未支付账单")
	}
	return &page.Items[0], nil
}

func invitationToken(link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("邀请链接非法: %w", err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || (parts[len(parts)-2] != "invite" && parts[len(parts)-2] != "invitations") {
		return "", fmt.Errorf("邀请链接格式不是预期的 /invite/<token>: %q", link)
	}
	token := parts[len(parts)-1]
	if token == "" {
		return "", errors.New("邀请链接缺少令牌")
	}
	return token, nil
}

func monthRange(now time.Time) (string, string) {
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1)
	return first.Format("2006-01-02"), last.Format("2006-01-02")
}

func printSummary(cfg config, s *summary) {
	fmt.Println("seed: 完成")
	fmt.Printf("  租户      %s（%s）\n", cfg.tenantSlug, s.TenantSlug)
	fmt.Printf("  owner     %s\n", s.Owner)
	fmt.Printf("  审批人    %s（admin）\n", s.Approver)
	fmt.Printf("  预算      %d 个（本次新建 %d）\n", s.Budgets, s.BudgetNew)
	fmt.Printf("  费用单    %d 张（本次新建 %d）：已报销 1、已批准 1、待审批 1、已驳回 1\n",
		s.Expenses, s.ExpenseNew)
	if s.Invoice != nil {
		fmt.Printf("  账单      %s / %s %d / 到期 %s（%s）\n",
			s.Invoice.Number, s.Invoice.Currency, s.Invoice.AmountMinor, s.Invoice.DueOn, s.Invoice.Status)
		fmt.Printf("  支付演示  go run ./cmd/mockpay -invoice %s -amount %d -currency %s\n",
			s.Invoice.ID, s.Invoice.AmountMinor, s.Invoice.Currency)
	}
	fmt.Println("  说明      口令与令牌不会打印；重复执行只会补齐缺失的数据")
}

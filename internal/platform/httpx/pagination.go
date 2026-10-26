package httpx

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// PageQuery 是所有列表接口共用的分页参数。
type PageQuery struct {
	Page     int `form:"page"     json:"page"`
	PageSize int `form:"page_size" json:"page_size"`
}

// Normalize 把非法或越界的参数收敛到合法范围。
// 不返回错误：分页参数越界是常态，前端传 page_size=1000 时给 100 比给 400 更友好。
func (q *PageQuery) Normalize() {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = DefaultPageSize
	}
	if q.PageSize > MaxPageSize {
		q.PageSize = MaxPageSize
	}
}

// Offset 必须在 Normalize 之后调用。
func (q PageQuery) Offset() int {
	return (q.Page - 1) * q.PageSize
}

// Page 是列表响应的标准形态。items 永远不为 null，前端不用判空。
type Page[T any] struct {
	Items    []T   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

func NewPage[T any](items []T, total int64, q PageQuery) Page[T] {
	q.Normalize()
	if items == nil {
		items = make([]T, 0)
	}
	return Page[T]{Items: items, Total: total, Page: q.Page, PageSize: q.PageSize}
}

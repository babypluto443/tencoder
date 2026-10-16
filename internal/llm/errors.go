// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package llm

type LLMError struct {
	Message string
}

func (e *LLMError) Error() string { return e.Message }

type AuthenticationError struct {
	Message string
}

func (e *AuthenticationError) Error() string { return e.Message }

type RateLimitError struct {
	Message    string
	RetryAfter string
}

func (e *RateLimitError) Error() string { return e.Message }

type NetworkError struct {
	Message string
}

func (e *NetworkError) Error() string { return e.Message }

type ContextTooLongError struct {
	Message string
}

func (e *ContextTooLongError) Error() string { return e.Message }

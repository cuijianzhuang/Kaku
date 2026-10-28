package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Settings actions (测试连接, 发送测试邮件, 设置 Webhook) show their result
// right under the button: the page calls them with fetch and draws the
// answer in place, and the last result is kept so it is still there when
// the page is opened again. Without JavaScript they redirect with a flash.

type actionResult struct {
	OK  bool      `json:"ok"`
	Msg string    `json:"message"`
	At  time.Time `json:"at"`
}

// When is the result's time for the page.
func (r *actionResult) When() string { return r.At.Local().Format("01-02 15:04") }

func wantsJSON(c *gin.Context) bool {
	return strings.Contains(c.GetHeader("Accept"), "application/json")
}

// actionDone records and answers the result of a settings action.
func (s *Server) actionDone(c *gin.Context, key, back string, err error, okMsg string) {
	r := actionResult{OK: err == nil, Msg: okMsg, At: s.now()}
	if err != nil {
		r.Msg = err.Error()
	}
	if b, e := json.Marshal(r); e == nil {
		s.st.SetValue("action:"+key, string(b))
	}
	if wantsJSON(c) {
		c.JSON(http.StatusOK, gin.H{"ok": r.OK, "message": r.Msg, "when": r.When()})
		return
	}
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	redirectMsg(c, back, okMsg)
}

// lastActions returns the stored results of the settings actions.
func (s *Server) lastActions() map[string]*actionResult {
	out := map[string]*actionResult{}
	for _, k := range []string{"mail", "telegram", "panel", "polaris"} {
		var r actionResult
		if v := s.st.Value("action:" + k); v != "" && json.Unmarshal([]byte(v), &r) == nil {
			out[k] = &r
		}
	}
	return out
}

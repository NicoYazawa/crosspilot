// Package http 提供 HTTP 接口层：路由、中间件与处理器。
package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// writeJSON 写出一个 JSON 响应。
//
// 一旦开始写 body 就无法再改状态码，因此序列化失败只能记录，不能再回头
// 返回 500。先序列化到内存再写，可以让「编码失败」发生在写头之前。
func writeJSON(w http.ResponseWriter, status int, payload any, logger *slog.Logger) {
	body, err := json.Marshal(payload)
	if err != nil {
		if logger != nil {
			logger.Error("响应体序列化失败", slog.Any("error", err))
		}
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

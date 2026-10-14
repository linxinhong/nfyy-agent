package httpapi

import (
	"time"

	"yuanpu.dev/server/internal/api"
)

// Public, non-sensitive sample content. Real Wiki routes will require identity
// and resource authorization before any organization content is exposed.
func wikiPreviewResponse() api.WikiPreviewResponse {
	updatedAt := time.Date(2026, time.October, 11, 0, 0, 0, 0, time.UTC)
	return api.WikiPreviewResponse{
		Preview: true,
		Entries: []api.WikiPreviewEntry{
			{
				Id: "knowledge-center", Title: "知识中心", Summary: "从本地工作中沉淀经验，再由中心治理和分发。",
				Body:  "## 定位\n\n中心保存经过授权的组织知识，负责版本、审核与分发。本机知识库保存个人内容，两者分开管理。\n\n这是一条公开示例词条，用于预览服务端 Wiki 界面。",
				Level: "示例 · 草稿", Experience: 12, UpdatedAt: updatedAt, Links: []string{"review-flow", "knowledge-delivery"},
			},
			{
				Id: "review-flow", Title: "词条审核", Summary: "词条经过提审、核验和发布后，才可进入组织共享范围。",
				Body:  "## 审核路径\n\n作者提交经验词条；审核人检查来源、适用范围和版本。发布与撤回都保留可追溯记录。\n\n正式流程将在账号和资源权限完成后接入。",
				Level: "示例 · 待审核", Experience: 28, UpdatedAt: updatedAt, Links: []string{"knowledge-center"},
			},
			{
				Id: "knowledge-delivery", Title: "按需分发", Summary: "客户端按任务请求必要词条，中心先检查权限。",
				Body:  "## 最小范围\n\n客户端只请求当前任务需要的知识。中心依据用户、团队和资源范围授权，再返回结构化数据。\n\n这个预览接口只返回公开示例，不提供组织数据。",
				Level: "示例 · 草稿", Experience: 8, UpdatedAt: updatedAt, Links: []string{"knowledge-center"},
			},
		},
	}
}

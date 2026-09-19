<!-- linux-compose-deploy:start -->
## Debian 单服务器部署闭环

- 部署配置：`.agents/linux-compose-deploy/config.md`（工具无关；遗留路径可继续读取，显式 --migrate-legacy 才迁移）。
- 项目实践：优先读取 `linux-compose-deploy` 技能的 `references/practices/workbuddy2api-panel.md`；不存在时再做通用调研。
- 仅在首次部署已成功且配置为 development 后，对运行时代码、依赖、配置、迁移、Dockerfile 或 Compose 变化自动执行部署闭环。
- 闭环顺序：本地测试与构建 → 冻结批准候选 → 部署唯一 Debian 12（遗留 11 需用户显式确认）amd64 目标 → 调用 `browser-fullstack-verify` 验证本次范围 → 记录部署结果与候选漂移 → 在项目或会话已授权范围内提交/推送。
- 纯文档或纯测试变化可跳过远程部署，但必须说明判定依据。
- 验收失败时修复重验或回滚；失败候选不得提交或推送。
- 保留与当前任务无关的工作区改动，只提交批准范围。
- SSH 连接元数据只从 `.deploy/target.env` 选中的用户级服务器 profile 或 SSH config 读取；密码只允许首次交互安装公钥，不写入仓库、日志、实践文件或回复。
<!-- linux-compose-deploy:end -->

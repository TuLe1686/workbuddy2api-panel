<!-- linux-compose-deploy:remote-verify:start -->
## Remote deployment mode

- mode: `remote`
- project_slug: `workbuddy2api-panel`
- public_frontend_url: `https://wb2api.99cy.edu.kg/panel/`
- api_base_url: `https://wb2api.99cy.edu.kg/v1`
- health_check_url: `https://wb2api.99cy.edu.kg/panel/`
- ssh_profile_source: `.deploy/target.env`
- remote_compose_dir: `/opt/apps/workbuddy2api-panel/current`
- compose_file: `docker-compose.yml`
- compose_project_name: `workbuddy2api-panel`
- database_assertion: `not-required`（无数据库；状态在 `shared/data/state.json` 与 `shared/auths`）
- test_account_source: 首次部署不导入 CodeBuddy 账号；面板 API 用服务器 `shared/config.json` 的 `panel_key`（管理面密钥；未设置时回落 `api_key`。SSH 进程内读取，不回显）
- writable_test_data_policy: `isolated-and-cleaned`
- core_pages:
  - `GET /panel/` → 200 HTML，标题含 WorkBuddy2API
  - `GET /panel/app.js` → 200
  - `GET /v1/models` 无密钥 → 401
  - `GET /healthz` → JSON `service=workbuddy2api`（空池可为 503）
- browser_driver: playwright.sync_api + 本机 `ms-playwright/chromium-1234`
- core_journeys: 密钥门真实提交 → 账号池空态（不导入上游账号）
- expected_redirects: `/panel` → 301 `/panel/`；`http://` → 301 `https://`
- console_error_allowlist:
  - 登录前提交前 `/panel/api/overview` 401（无 Bearer，密钥门预期）
  - `/favicon.ico` 404（站点无图标）
- failed_request_allowlist: []
- last_verified_revision: commit `4e792d3` / image `workbuddy2api-panel:v1.12.1`（2026-09-28：v1.12.0 扩展验收同前全部通过；v1.12.1 = 11140 分野 + 夜猫子治本，演练 167/135 + 解冻后 167/167 + 真实 chat 200）
<!-- 更新: 2026-09-20 同步远程验收 URL、空池旅程与控制台 allowlist -->
<!-- 更新: 2026-09-25 面板闸门密钥改 panel_key（未设置时回落 api_key），对齐当前鉴权实现 -->
<!-- 更新: 2026-09-27 上游 v1.11.7 合并上线 v1.11.11 浏览器验收通过 -->
<!-- 更新: 2026-09-27 v1.12.0 扩展验收（tooltip/宽度/日志过滤/限流标签 stub）通过 -->
<!-- 更新: 2026-09-28 v1.12.1（11140 内容审核分野 + 夜猫子治本）上线，32 误禁号解冻，167/167 恢复 -->
<!-- linux-compose-deploy:remote-verify:end -->

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
- test_account_source: 首次部署不导入 CodeBuddy 账号；面板 API 用服务器 `shared/config.json` 的 `api_key`（SSH 进程内读取，不回显）
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
- last_verified_revision: candidate_tree `6c9a0de642c5b6238e493fd9ee581ff8eb72ab1b` / image `workbuddy2api-panel:v1.11.0`
<!-- 更新: 2026-09-20 同步远程验收 URL、空池旅程与控制台 allowlist -->
<!-- linux-compose-deploy:remote-verify:end -->

# Linux Compose Deploy 项目配置

schema_version: 1

## 项目

- project_slug: `workbuddy2api-panel`
- repository: `https://github.com/linguo2625469/workbuddy2api-panel`
- pinned_version: `v1.11.0` (`b4245a833fb9189ba816d35cfd5c6a523ca432ca`)
- deployment_mode: `development`
- initial_deployment_status: `completed`
- auto_deploy: `runtime-only`

## Compose

- compose_file: `docker-compose.yml`
- compose_project_name: `workbuddy2api-panel`
- services: `wb2api`
- health_checks: 容器探活 `GET /panel/`（空账号时 `/healthz` 为 503，不能当启动门禁）；业务身份 `GET /healthz` 体含 `service=workbuddy2api`
- persistent_resources: `shared/auths`、`shared/data`、`shared/config.json`

## 当前唯一目标选择

- target_profile: `us-biz2`
- target_selector: `.deploy/target.env`
- target_profiles_dir: `user-level default or LINUX_COMPOSE_DEPLOY_HOME/targets`
- expected_os: `debian 12（遗留 11 需用户显式确认）`
- expected_arch: `amd64/x86_64`
- deploy_root: `/opt/apps/workbuddy2api-panel`（符号链接到 `/www/apps/workbuddy2api-panel`）
- data_root: `/www/apps/workbuddy2api-panel`
- public_url: `https://wb2api.99cy.edu.kg`
- api_base_url: `https://wb2api.99cy.edu.kg/v1`
- listen: `127.0.0.1:7863`

## 改动触发

- runtime_paths: `cmd/`、`internal/`、`scripts/`、`Dockerfile`、`docker-compose.yml`、`go.mod`、`go.sum`、`login.sh`、`signin.sh`、`credit.sh`、`config.example.json`
- exclude_paths: `README.md`、`LICENSE`、`AGENTS.md`、`.agents/`、纯测试文档

## Git

- push_after_remote_verification: `false`
- branch: `deploy/us-biz2-v1.11.0`
- git_authorization: `follow existing project or session authorization; record separately from deployment`

> `target_profile` 必须与 `.deploy/target.env` 的 `TARGET_PROFILE` 一致；旧式 inline target.env 仅用于兼容，不在新项目生成。
>
> 首次完整部署并验收成功后，把 `deployment_mode` 改为 `development`、`initial_deployment_status` 改为 `completed`、`auto_deploy` 改为 `runtime-only`。

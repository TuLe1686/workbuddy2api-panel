# workbuddy2api-panel 页面元素

## `/panel/` 密钥门

- 标题：WorkBuddy2API · 控制台
- 对话框：`#keyVeil`「需要访问密钥」
- 输入：`#keyInput` type=password placeholder=`api_key`
- 按钮：`#btnKey`「进入」
- 错误：`#keyErr`「密钥不正确，请重试。」（默认 hidden）

## `/panel/` 账号池（登录后默认视图）

- 顶栏：`#ttl` 账号池；`#btnAdd` 添加账号；`#btnRefresh` 刷新
- 统计：`#sTotal` 账号总数、`#sHealthy` 可用、`#sCooling` 冷却中、`#sDisabled` 已禁用
- 空态文案：账号池是空的
- 侧栏版本：`#navVer` 形如 `v1.11.0-panel`；`#navRedis` 本地内存 / Redis 镜像
- 侧栏状态：`#navState` 待添加账号 / 服务正常 / 无可用账号

<!-- 更新: 2026-09-20 首次部署浏览器验收同步 -->

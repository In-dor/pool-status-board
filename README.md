# Pool Status Board（双号池额度与健康度看板）

轻量级只读公开看板（Go 单二进制 + 内嵌零依赖静态前端），用于统一汇总并脱敏展示 **Antigravity（CLIProxyAPI / CPAMP / lamplighter）** 与 **AIStudio2API（Playground 每日额度）** 双号池的实时可用余量、近期健康度与周期消耗趋势。

## 核心特性

- **零上游推理消耗 & 严格脱敏**：后台定时只读采集并缓存内存快照，所有公开接口均不包含真实邮箱、凭证文件名、`auth_index`、`project_id`、Cookie 或任何密钥。
- **CPAMP 同款近期健康条**：复用 `auth-files` 接口 20 × 10 分钟分桶数据（近 200 分钟），三色 RGB 线性插值胶囊条 + 整体成功率与时段详情。
- **周期燃烧分析图（Cycle Burn Chart）**：内存增量缓存近 36 天 `lamplighter` 采样历史，支持 `5 小时 / 7 天` 窗口与多跨度切换、5 级健康色带时间轴、重置断线平滑曲线、匀速参考线、按当前速度延长线预测，以及桌面悬停与移动端全宽触控读数条。
- **AIStudio2API 短板熔断核算**：按每日 16:00（北京时间）周期核算全部模型次数与 Token 双维度短板余量、冷却倒计时，并附带本轮分小时消耗趋势柱状图。
- **可选口令门禁**：通过 `.env` 中的 `VIEW_PASSCODE` 保护 `/api/snapshot` 与 `/api/antigravity/history`（常量时间比对）。

## 项目结构

```text
Pool-Status-Board/
├── main.go              # HTTP 服务、定时刷新、口令校验与 /api/snapshot、/api/antigravity/history 路由
├── types.go             # 严格脱敏的公开 API DTO 定义
├── cpa_collector.go     # Antigravity 凭证、lamplighter 36 天历史缓存与 CPAMP 统计采集器
├── ais_collector.go     # AIStudio2API 账号、冷却、分时统计与短板熔断采集器
├── sanitize.go          # 匿名代号排序、档位归一化与数值工具函数
├── quotas.json          # AIStudio2API 各档位每日模型配额表（支持热重载）
├── web/index.html       # 单文件零依赖响应式前端（浅/深色主题 + 内联 SVG 图表）
├── Dockerfile           # 多阶段静态编译镜像
├── docker-compose.yml   # 容器编排配置
├── .env.example         # 环境变量模板
└── .gitignore           # 忽略 secrets/ 与 .env 等敏感文件
```

## 快速部署与运维

### 1. 准备本地配置

```bash
cp .env.example .env
chmod 600 .env
# 按需在 .env 中设置 VIEW_PASSCODE（留空则关闭口令保护）

mkdir -p secrets
chmod 700 secrets
# 将 CPAMP Admin Key 写入 secrets/cpamp_admin_key（权限建议 600），支持运行时自动热加载
```

### 2. 构建与更新

```bash
docker compose up -d --build
```

### 3. 日常配置热更新

- **修改模型配额**：直接编辑 `quotas.json`，采集器通过文件 `mtime + size` 自动热重载，无需重启容器。
- **更新 CPAMP Admin Key**：替换 `secrets/cpamp_admin_key`，后台每 3 秒检测变更并立即触发一次快照刷新。
- **修改访问口令**：更新 `.env` 中的 `VIEW_PASSCODE` 后执行 `docker compose up -d` 生效。

### 4. 查看健康状态与日志

```bash
docker compose ps
docker compose logs -f --tail 50
curl -s http://127.0.0.1:18380/health
```

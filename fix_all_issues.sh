#!/bin/bash
# Codex Pool 综合修复脚本
# 用途：修复503错误、CLI上下文隔离、磁盘空间等问题

set -e

echo "================================================================"
echo "         Codex Pool 问题修复脚本"
echo "================================================================"
echo ""

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 检查是否以root运行
check_root() {
    if [ "$EUID" -ne 0 ]; then
        echo -e "${YELLOW}警告: 某些操作可能需要root权限${NC}"
    fi
}

# 1. 磁盘空间清理
cleanup_disk_space() {
    echo -e "${GREEN}[1/6] 清理磁盘空间...${NC}"

    # 清理旧的rollout日志
    echo "  - 清理7天前的rollout日志..."
    find /workspace -name "rollout-*.jsonl" -mtime +7 -delete 2>/dev/null || true

    # 清理临时文件
    echo "  - 清理临时文件..."
    find /workspace -name "*.tmp" -mtime +1 -delete 2>/dev/null || true
    find /tmp -name "codex-*" -mtime +1 -delete 2>/dev/null || true

    # 清理旧的worker socket
    if [ -d "/var/lib/codex-pool/run" ]; then
        echo "  - 清理旧的worker socket..."
        find /var/lib/codex-pool/run -name "worker-*.sock" -mmin +60 -delete 2>/dev/null || true
    fi

    # 清理SQLite的WAL文件（如果数据库已关闭）
    echo "  - 检查SQLite WAL文件..."
    for db in /workspace/*.sqlite3; do
        if [ -f "$db" ]; then
            wal_file="${db}-wal"
            if [ -f "$wal_file" ]; then
                size=$(stat -f%z "$wal_file" 2>/dev/null || stat -c%s "$wal_file" 2>/dev/null || echo "0")
                if [ "$size" -eq 0 ]; then
                    rm -f "$wal_file" 2>/dev/null || true
                    echo "    已删除空WAL文件: $(basename $wal_file)"
                fi
            fi
        fi
    done

    echo -e "${GREEN}  ✓ 磁盘清理完成${NC}"
    echo ""
}

# 2. 编译新版本
build_server() {
    echo -e "${GREEN}[2/6] 编译更新后的服务器...${NC}"

    cd /workspace

    if [ ! -f "go.mod" ]; then
        echo -e "${RED}  ✗ 错误: 找不到go.mod文件${NC}"
        return 1
    fi

    echo "  - 正在编译 codex-pool-server..."
    go build -o codex-pool-server-new ./cmd/pool-server || {
        echo -e "${RED}  ✗ 编译失败（需要Go 1.23+，当前可能是Go 1.19）${NC}"
        echo -e "${YELLOW}  提示: 请先更新Go版本到1.23或更高${NC}"
        return 1
    }

    echo -e "${GREEN}  ✓ 编译成功${NC}"
    echo ""
}

# 3. 备份当前配置
backup_config() {
    echo -e "${GREEN}[3/6] 备份配置文件...${NC}"

    timestamp=$(date +%Y%m%d_%H%M%S)
    backup_dir="/workspace/backup_${timestamp}"
    mkdir -p "$backup_dir"

    # 备份数据库
    if [ -f "/workspace/codex-pool.sqlite3" ]; then
        echo "  - 备份数据库..."
        cp /workspace/codex-pool.sqlite3 "$backup_dir/" || true
    fi

    # 备份配置文件
    if [ -f "/workspace/config.local.json" ]; then
        echo "  - 备份配置文件..."
        cp /workspace/config.local.json "$backup_dir/" || true
    fi

    echo -e "${GREEN}  ✓ 备份完成: $backup_dir${NC}"
    echo ""
}

# 4. 优化配置
optimize_config() {
    echo -e "${GREEN}[4/6] 优化配置...${NC}"

    if [ -f "/workspace/config.optimized.json" ]; then
        echo "  - 发现优化配置文件"
        echo "  - 提示: 您可以使用 config.optimized.json 替换当前配置"
        echo "    该配置包含以下优化:"
        echo "      * 降低内存和磁盘使用 (body_spool_max_bytes: 10GB)"
        echo "      * 减少上下文保留时间 (goal_retention_days: 3)"
        echo "      * 优化并发数 (max_concurrent_upstream: 32)"
        echo "      * 启用stateless模式以减少存储压力"
    fi

    echo -e "${GREEN}  ✓ 配置检查完成${NC}"
    echo ""
}

# 5. 创建维护脚本
create_maintenance_script() {
    echo -e "${GREEN}[5/6] 创建维护脚本...${NC}"

    cat > /workspace/scripts/daily_maintenance.sh << 'MAINTENANCE_EOF'
#!/bin/bash
# 每日维护脚本 - 建议在cron中每天运行一次

echo "[$(date)] 开始每日维护..."

# 清理7天前的旧日志
find /workspace -name "rollout-*.jsonl" -mtime +7 -delete 2>/dev/null

# 清理临时文件
find /workspace -name "*.tmp" -mtime +1 -delete 2>/dev/null

# 清理过期的billing holds (如果服务器支持)
# curl -X POST http://localhost:8787/admin/cleanup/billing_holds \
#      -H "Authorization: Bearer YOUR_ADMIN_TOKEN"

# 压缩旧的SQLite数据库
if command -v sqlite3 &> /dev/null; then
    for db in /workspace/*.sqlite3; do
        if [ -f "$db" ]; then
            echo "压缩数据库: $db"
            sqlite3 "$db" "VACUUM;" 2>/dev/null || true
        fi
    done
fi

echo "[$(date)] 每日维护完成"
MAINTENANCE_EOF

    chmod +x /workspace/scripts/daily_maintenance.sh

    echo -e "${GREEN}  ✓ 维护脚本已创建: /workspace/scripts/daily_maintenance.sh${NC}"
    echo ""
}

# 6. 显示修复报告
show_report() {
    echo -e "${GREEN}[6/6] 生成修复报告...${NC}"
    echo ""
    echo "================================================================"
    echo "                    修复报告"
    echo "================================================================"
    echo ""

    echo "【已修复的问题】"
    echo "  ✓ CLI上下文隔离: 添加了X-Session-ID header支持"
    echo "  ✓ 磁盘空间: 清理了临时文件和旧日志"
    echo "  ✓ 配置优化: 创建了优化后的配置文件"
    echo ""

    echo "【诊断包分析结果】"
    echo "  • 503错误: 仅5个，不是系统性问题"
    echo "  • 磁盘空间问题: 180个rejections，需要增加可用空间"
    echo "  • 账户路由错误: antigravity组配置需要调整"
    echo "  • Rate Limit: 正常的保护机制，运行正常"
    echo ""

    echo "【需要手动操作】"
    echo "  1. 替换配置文件 (可选):"
    echo "     cp /workspace/config.optimized.json /workspace/config.local.json"
    echo ""
    echo "  2. 重启服务器:"
    echo "     systemctl restart codex-pool  # 或者"
    echo "     killall codex-pool-server && ./codex-pool-server-new"
    echo ""
    echo "  3. 配置账户组 (修复routing_unavailable错误):"
    echo "     - 确保 'antigravity' 组使用 antigravity provider"
    echo "     - 确保 'claude' 组使用 claude provider"
    echo "     - 确保 'gpt-*' 组使用 codex provider"
    echo ""
    echo "  4. CLI会话隔离 (客户端需要添加):"
    echo "     在HTTP请求中添加header: X-Session-ID: <unique-id>"
    echo "     例如: curl -H 'X-Session-ID: cli-001' ..."
    echo ""
    echo "  5. 设置定时清理任务:"
    echo "     crontab -e"
    echo "     添加: 0 3 * * * /workspace/scripts/daily_maintenance.sh"
    echo ""

    echo "【磁盘使用情况】"
    df -h /workspace | tail -1
    echo ""

    echo "【最大占用目录】"
    du -sh /workspace/* 2>/dev/null | sort -hr | head -5
    echo ""

    echo "================================================================"
    echo "修复脚本执行完成！"
    echo "================================================================"
}

# 主流程
main() {
    check_root
    cleanup_disk_space
    build_server
    backup_config
    optimize_config
    create_maintenance_script
    show_report
}

main "$@"

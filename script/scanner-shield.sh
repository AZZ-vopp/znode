#!/bin/sh
set -eu
umask 077

TABLE='zboard_scanner_shield'
RULE_DIR='/etc/zboard'
RULE_FILE="$RULE_DIR/scanner-shield.nft"
APPLY_FILE='/usr/local/sbin/zboard-scanner-shield-apply'
SERVICE_FILE='/etc/systemd/system/zboard-scanner-shield.service'
ACTION="${1:-install}"

die() {
    printf 'Lỗi: %s\n' "$1" >&2
    exit 1
}

require_root() {
    [ "$(id -u)" -eq 0 ] || die 'phải chạy bằng root.'
    command -v nft >/dev/null 2>&1 || die 'chưa cài nftables.'
}

write_rules() {
    destination=$1
    cat > "$destination" <<'EOF'
table inet zboard_scanner_shield {
    set censys_ipv4 {
        type ipv4_addr
        flags interval
        elements = {
            66.132.159.0/24, 66.132.148.0/24, 66.132.153.0/24,
            66.132.224.0/24, 66.132.186.0/24, 66.132.195.0/24,
            66.132.172.0/24, 162.142.125.0/24, 167.94.138.0/24,
            167.94.145.0/24, 167.94.146.0/24, 167.248.133.0/24,
            199.45.154.0/24, 199.45.155.0/24, 206.168.34.0/24,
            206.168.35.0/24
        }
    }

    set censys_ipv6 {
        type ipv6_addr
        flags interval
        elements = {
            2602:80d:1000:b0cc:e::/80,
            2620:96:e000:b0cc:e::/80,
            2602:80d:1003::/112,
            2602:80d:1004::/112
        }
    }

    chain input {
        type filter hook input priority -5; policy accept;
        ip saddr @censys_ipv4 counter drop
        ip6 saddr @censys_ipv6 counter drop
    }
}
EOF
}

install_boot_service() {
    cat > "$APPLY_FILE" <<'EOF'
#!/bin/sh
set -eu
case "${1:-apply}" in
    apply)
        nft delete table inet zboard_scanner_shield 2>/dev/null || true
        exec nft -f /etc/zboard/scanner-shield.nft
        ;;
    remove)
        nft delete table inet zboard_scanner_shield 2>/dev/null || true
        ;;
    *) exit 64 ;;
esac
EOF
    chmod 0700 "$APPLY_FILE"

    if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
        cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=ZBoard scanner opt-out firewall table
After=network-pre.target nftables.service ufw.service firewalld.service
Before=znode.service

[Service]
Type=oneshot
ExecStart=$APPLY_FILE apply
ExecStop=$APPLY_FILE remove
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
        chmod 0600 "$SERVICE_FILE"
        systemctl daemon-reload
        systemctl enable zboard-scanner-shield.service >/dev/null
        systemctl restart zboard-scanner-shield.service
        return 0
    fi

    printf '%s\n' "Chú ý: hệ thống không dùng systemd; hãy chạy $APPLY_FILE apply sau mỗi lần khởi động." >&2
}

install_shield() {
    require_root
    temporary=$(mktemp /run/zboard-scanner-shield.XXXXXX)
    backup=$(mktemp /run/zboard-scanner-shield-backup.XXXXXX)
    check_file=$(mktemp /run/zboard-scanner-shield-check.XXXXXX)
    trap 'rm -f "$temporary" "$backup" "$check_file"' EXIT HUP INT TERM

    write_rules "$temporary"
    sed "s/zboard_scanner_shield/zboard_scanner_shield_check_$$/g" "$temporary" > "$check_file"
    nft -c -f "$check_file" || die 'rule nftables không hợp lệ.'
    nft list table inet "$TABLE" > "$backup" 2>/dev/null || :
    nft delete table inet "$TABLE" 2>/dev/null || :
    if ! nft -f "$temporary"; then
        [ -s "$backup" ] && nft -f "$backup" 2>/dev/null || true
        die 'không thể kích hoạt rule; rule cũ đã được khôi phục nếu có.'
    fi

    mkdir -p "$RULE_DIR"
    install -m 0600 "$temporary" "$RULE_FILE"
    install_boot_service

    printf '%s\n' 'Đã chặn dải quét Censys ở firewall cho mọi cổng.'
    printf '%s\n' 'Hãy chạy script này trên panel và từng VPS ZNode.'
    printf '%s\n' 'Dữ liệu Censys cũ có thể cần 24-48 giờ mới biến mất.'
}

remove_shield() {
    require_root
    if command -v systemctl >/dev/null 2>&1 && [ -e "$SERVICE_FILE" ]; then
        systemctl disable --now zboard-scanner-shield.service >/dev/null 2>&1 || true
        rm -f "$SERVICE_FILE"
        systemctl daemon-reload >/dev/null 2>&1 || true
    fi
    nft delete table inet "$TABLE" 2>/dev/null || true
    rm -f "$RULE_FILE" "$APPLY_FILE"
    printf '%s\n' 'Đã gỡ ZBoard scanner shield.'
}

show_status() {
    require_root
    nft list table inet "$TABLE"
}

case "$ACTION" in
    install|--install) install_shield ;;
    remove|uninstall|--remove|--uninstall) remove_shield ;;
    status|--status) show_status ;;
    *) die 'dùng: scanner-shield.sh [install|status|remove]' ;;
esac

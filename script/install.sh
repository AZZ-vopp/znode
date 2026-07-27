#!/bin/bash

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

cur_dir=$(pwd)

# check root
[[ $EUID -ne 0 ]] && echo -e "${red}Lỗi:${plain} Phải chạy script này bằng tài khoản root!\n" && exit 1

# check os
if [[ -f /etc/redhat-release ]]; then
    release="centos"
elif cat /etc/issue | grep -Eqi "alpine"; then
    release="alpine"
elif cat /etc/issue | grep -Eqi "debian"; then
    release="debian"
elif cat /etc/issue | grep -Eqi "ubuntu"; then
    release="ubuntu"
elif cat /etc/issue | grep -Eqi "centos|red hat|redhat|rocky|alma|oracle linux"; then
    release="centos"
elif cat /proc/version | grep -Eqi "debian"; then
    release="debian"
elif cat /proc/version | grep -Eqi "ubuntu"; then
    release="ubuntu"
elif cat /proc/version | grep -Eqi "centos|red hat|redhat|rocky|alma|oracle linux"; then
    release="centos"
elif cat /proc/version | grep -Eqi "arch"; then
    release="arch"
else
    echo -e "${red}Không xác định được phiên bản hệ điều hành, vui lòng liên hệ tác giả script!${plain}\n" && exit 1
fi

########################
# Phân tích tham số
########################
VERSION_ARG=""
API_HOST_ARG=""
NODE_ID_ARG=""
API_KEY_ARG=""
AGENT_ID_ARG=""
AGENT_TOKEN_ARG=""
POLL_INTERVAL_ARG="15"
RELEASE_REPO_ARG="${ZNODE_RELEASE_REPO:-AZZ-vopp/znode}"
RELEASE_BRANCH_ARG="${ZNODE_RELEASE_BRANCH:-main}"
GEODATA_BASE_URL="${ZNODE_GEODATA_URL:-https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download}"

install_geodata() (
    local destination="/etc/znode"
    local temporary
    temporary=$(mktemp -d) || return 1
    trap 'rm -rf "$temporary"' EXIT

    mkdir -p "$destination"
    for file in geoip.dat geosite.dat; do
        if [[ -s "/usr/local/znode/$file" ]] && [[ $(wc -c < "/usr/local/znode/$file") -ge 1024 ]]; then
            install -m 0644 "/usr/local/znode/$file" "$destination/$file"
            continue
        fi
        echo -e "${yellow}Không có $file trong gói phát hành, đang tải dữ liệu định tuyến mới nhất...${plain}"
        if ! curl --fail --location --silent --show-error --retry 3 --connect-timeout 15 \
            "$GEODATA_BASE_URL/$file" -o "$temporary/$file"; then
            echo -e "${red}Không thể tải $file.${plain}"
            return 1
        fi
        if [[ $(wc -c < "$temporary/$file") -lt 1024 ]]; then
            echo -e "${red}$file tải về không hợp lệ hoặc đã bị cắt ngắn.${plain}"
            return 1
        fi
        install -m 0644 "$temporary/$file" "$destination/$file"
    done
    echo -e "${green}Đã cài geoip.dat và geosite.dat vào $destination.${plain}"
)

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --api-host)
                API_HOST_ARG="$2"; shift 2 ;;
            --node-id)
                NODE_ID_ARG="$2"; shift 2 ;;
            --api-key)
                API_KEY_ARG="$2"; shift 2 ;;
            --agent-id)
                AGENT_ID_ARG="$2"; shift 2 ;;
            --agent-token)
                AGENT_TOKEN_ARG="$2"; shift 2 ;;
            --poll-interval)
                POLL_INTERVAL_ARG="$2"; shift 2 ;;
            --release-repo)
                RELEASE_REPO_ARG="$2"; shift 2 ;;
            --release-branch)
                RELEASE_BRANCH_ARG="$2"; shift 2 ;;
            -h|--help)
                echo "Agent: $0 [version] --api-host URL --agent-id ID --agent-token TOKEN [--poll-interval SEC] [--release-repo OWNER/REPO]"
                echo "Legacy: $0 [version] --api-host URL --node-id ID --api-key KEY"
                exit 0 ;;
            --*)
                echo "Tham số không xác định: $1"; exit 1 ;;
            *)
                # Tương thích với tham số vị trí đầu tiên dùng làm phiên bản
                if [[ -z "$VERSION_ARG" ]]; then
                    VERSION_ARG="$1"; shift
                else
                    shift
                fi ;;
        esac
    done
}

validate_release_source() {
    if [[ ! "$RELEASE_REPO_ARG" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
        echo -e "${red}Invalid --release-repo; expected OWNER/REPO.${plain}"
        exit 1
    fi
    if [[ ! "$RELEASE_BRANCH_ARG" =~ ^[A-Za-z0-9._/-]+$ ]]; then
        echo -e "${red}Invalid --release-branch.${plain}"
        exit 1
    fi
}

validate_agent_args() {
    if [[ -z "$AGENT_ID_ARG" && -z "$AGENT_TOKEN_ARG" ]]; then
        return 0
    fi
    if [[ -z "$API_HOST_ARG" || -z "$AGENT_ID_ARG" || -z "$AGENT_TOKEN_ARG" ]]; then
        echo -e "${red}Agent install requires --api-host, --agent-id and --agent-token together.${plain}"
        exit 1
    fi
    if [[ ! "$API_HOST_ARG" =~ ^https?:// ]] ||
       [[ "$API_HOST_ARG" == *\"* ]] ||
       [[ "$API_HOST_ARG" == *\\* ]] ||
       [[ "$API_HOST_ARG" =~ [[:space:]] ]]; then
        echo -e "${red}Invalid --api-host; expected an HTTP(S) URL without quotes, backslashes or spaces.${plain}"
        exit 1
    fi
    if [[ ! "$AGENT_ID_ARG" =~ ^[A-Za-z0-9._~-]+$ ]]; then
        echo -e "${red}Invalid agent ID format.${plain}"
        exit 1
    fi
    if [[ ! "$AGENT_TOKEN_ARG" =~ ^[A-Za-z0-9._~-]+$ ]]; then
        echo -e "${red}Invalid agent token format.${plain}"
        exit 1
    fi
    if [[ ! "$POLL_INTERVAL_ARG" =~ ^[0-9]+$ ]] || (( POLL_INTERVAL_ARG < 5 || POLL_INTERVAL_ARG > 3600 )); then
        echo -e "${red}Invalid --poll-interval; expected 5..3600 seconds.${plain}"
        exit 1
    fi
}

validate_existing_agent_binding() {
    if [[ -z "$AGENT_ID_ARG" || ! -f /etc/znode/config.json ]]; then
        return 0
    fi

    local existing_agent_id
    existing_agent_id=$(sed -n 's/^[[:space:]]*"AgentID"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' /etc/znode/config.json | head -n 1)
    if [[ -z "$existing_agent_id" ]]; then
        echo -e "${red}This VPS already has a manual znode config. Back it up and remove /etc/znode/config.json before enrolling an agent.${plain}"
        exit 1
    fi
    if [[ "$existing_agent_id" != "$AGENT_ID_ARG" ]]; then
        echo -e "${red}This VPS is already enrolled as agent ${existing_agent_id}; refusing to replace it with ${AGENT_ID_ARG}.${plain}"
        exit 1
    fi

    if [[ ! "$AGENT_TOKEN_ARG" =~ ^[A-Za-z0-9._~-]+$ ]]; then
        echo -e "${red}Invalid agent token format.${plain}"
        exit 1
    fi
    local existing_agent_token
    existing_agent_token=$(sed -n 's/^[[:space:]]*"AgentToken"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' /etc/znode/config.json | head -n 1)
    if [[ -z "$existing_agent_token" ]]; then
        echo -e "${red}Existing agent config has no AgentToken; refusing to edit it automatically.${plain}"
        exit 1
    fi
    if [[ "$existing_agent_token" != "$AGENT_TOKEN_ARG" ]]; then
        local updated_config
        updated_config=$(mktemp /etc/znode/config.json.XXXXXX)
        sed "s|^\([[:space:]]*\"AgentToken\"[[:space:]]*:[[:space:]]*\"\)[^\"]*\(\".*\)$|\1${AGENT_TOKEN_ARG}\2|" /etc/znode/config.json > "$updated_config"
        if ! grep -q "\"AgentToken\"[[:space:]]*:[[:space:]]*\"${AGENT_TOKEN_ARG}\"" "$updated_config"; then
            rm -f "$updated_config"
            echo -e "${red}Could not update AgentToken safely.${plain}"
            exit 1
        fi
        chmod 600 "$updated_config"
        mv -f "$updated_config" /etc/znode/config.json
        echo -e "${green}Updated the token for existing agent ${existing_agent_id}.${plain}"
    fi

    echo -e "${green}This VPS is already enrolled with the selected agent; keeping its identity and other settings.${plain}"
}

arch=$(uname -m)

if [[ $arch == "x86_64" || $arch == "x64" || $arch == "amd64" ]]; then
    arch="64"
elif [[ $arch == "aarch64" || $arch == "arm64" ]]; then
    arch="arm64-v8a"
elif [[ $arch == "s390x" ]]; then
    arch="s390x"
else
    arch="64"
    echo -e "${red}Không xác định được kiến trúc, sử dụng kiến trúc mặc định: ${arch}${plain}"
fi

if [ "$(getconf WORD_BIT)" != '32' ] && [ "$(getconf LONG_BIT)" != '64' ] ; then
    echo "Phần mềm không hỗ trợ hệ điều hành 32-bit (x86). Vui lòng sử dụng hệ điều hành 64-bit (x86_64); nếu phát hiện sai, hãy liên hệ tác giả."
    exit 2
fi

# os version
if [[ -f /etc/os-release ]]; then
    os_version=$(awk -F'[= ."]' '/VERSION_ID/{print $3}' /etc/os-release)
fi
if [[ -z "$os_version" && -f /etc/lsb-release ]]; then
    os_version=$(awk -F'[= ."]+' '/DISTRIB_RELEASE/{print $2}' /etc/lsb-release)
fi

if [[ x"${release}" == x"centos" ]]; then
    if [[ ${os_version} -le 6 ]]; then
        echo -e "${red}Vui lòng sử dụng CentOS 7 hoặc phiên bản mới hơn!${plain}\n" && exit 1
    fi
    if [[ ${os_version} -eq 7 ]]; then
        echo -e "${red}Lưu ý: CentOS 7 không hỗ trợ giao thức Hysteria 1/2!${plain}\n"
    fi
elif [[ x"${release}" == x"ubuntu" ]]; then
    if [[ ${os_version} -lt 16 ]]; then
        echo -e "${red}Vui lòng sử dụng Ubuntu 16 hoặc phiên bản mới hơn!${plain}\n" && exit 1
    fi
elif [[ x"${release}" == x"debian" ]]; then
    if [[ ${os_version} -lt 8 ]]; then
        echo -e "${red}Vui lòng sử dụng Debian 8 hoặc phiên bản mới hơn!${plain}\n" && exit 1
    fi
fi

install_base() {
    # Kiểm tra và cài gói theo lô để giảm số lần gọi hệ thống
    need_install_apt() {
        local packages=("$@")
        local missing=()

        # Kiểm tra theo lô các gói đã cài
        local installed_list=$(dpkg-query -W -f='${Package}\n' 2>/dev/null | sort)

        for p in "${packages[@]}"; do
            if ! echo "$installed_list" | grep -q "^${p}$"; then
                missing+=("$p")
            fi
        done

        if [[ ${#missing[@]} -gt 0 ]]; then
            echo "Đang cài các gói còn thiếu: ${missing[*]}"
            apt-get update -y >/dev/null 2>&1
            DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing[@]}" >/dev/null 2>&1
        fi
    }

    need_install_yum() {
        local packages=("$@")
        local missing=()

        # Kiểm tra theo lô các gói đã cài
        local installed_list=$(rpm -qa --qf '%{NAME}\n' 2>/dev/null | sort)

        for p in "${packages[@]}"; do
            if ! echo "$installed_list" | grep -q "^${p}$"; then
                missing+=("$p")
            fi
        done

        if [[ ${#missing[@]} -gt 0 ]]; then
            echo "Đang cài các gói còn thiếu: ${missing[*]}"
            yum install -y "${missing[@]}" >/dev/null 2>&1
        fi
    }

    need_install_apk() {
        local packages=("$@")
        local missing=()

        # Kiểm tra theo lô các gói đã cài
        local installed_list=$(apk info 2>/dev/null | sort)

        for p in "${packages[@]}"; do
            if ! echo "$installed_list" | grep -q "^${p}$"; then
                missing+=("$p")
            fi
        done

        if [[ ${#missing[@]} -gt 0 ]]; then
            echo "Đang cài các gói còn thiếu: ${missing[*]}"
            apk add --no-cache "${missing[@]}" >/dev/null 2>&1
        fi
    }

    # Cài tất cả gói bắt buộc trong một lượt
    if [[ x"${release}" == x"centos" ]]; then
        # Kiểm tra và cài epel-release
        if ! rpm -q epel-release >/dev/null 2>&1; then
            echo "Đang cài kho EPEL..."
            yum install -y epel-release >/dev/null 2>&1
        fi
        need_install_yum wget curl unzip tar cronie socat ca-certificates pv nano
        update-ca-trust force-enable >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"alpine" ]]; then
        need_install_apk wget curl unzip tar socat ca-certificates pv nano
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"debian" ]]; then
        need_install_apt wget curl unzip tar cron socat ca-certificates pv nano
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"ubuntu" ]]; then
        need_install_apt wget curl unzip tar cron socat ca-certificates pv nano
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"arch" ]]; then
        echo "Đang cập nhật cơ sở dữ liệu gói..."
        pacman -Sy --noconfirm >/dev/null 2>&1
        # --needed sẽ bỏ qua các gói đã cài
        echo "Đang cài các gói bắt buộc..."
        pacman -S --noconfirm --needed wget curl unzip tar cronie socat ca-certificates pv nano >/dev/null 2>&1
    fi
}

# 0: running, 1: not running, 2: not installed
check_status() {
    if [[ ! -f /usr/local/znode/znode ]]; then
        return 2
    fi
    if [[ x"${release}" == x"alpine" ]]; then
        temp=$(service znode status | awk '{print $3}')
        if [[ x"${temp}" == x"started" ]]; then
            return 0
        else
            return 1
        fi
    else
        temp=$(systemctl status znode | grep Active | awk '{print $3}' | cut -d "(" -f2 | cut -d ")" -f1)
        if [[ x"${temp}" == x"running" ]]; then
            return 0
        else
            return 1
        fi
    fi
}

generate_znode_config() {
        local api_host="$1"
        local node_id="$2"
        local api_key="$3"

        mkdir -p /etc/znode >/dev/null 2>&1
        cat > /etc/znode/config.json <<EOF
{
    "Log": {
        "Level": "warning",
        "Output": "",
        "Access": "none"
    },
    "ConnectionConfig": {
        "Handshake": 4,
        "ConnIdle": 30,
        "UplinkOnly": 2,
        "DownlinkOnly": 4,
        "BufferSize": 16,
        "DisableUDPContentSniffing": true
    },
    "Nodes": [
        {
            "ApiHost": "${api_host}",
            "NodeID": ${node_id},
            "ApiKey": "${api_key}",
            "Timeout": 15,
            "GlobalDeviceLimitConfig": {
                "Enable": true,
                "SyncEnabled": true,
                "SyncChannel": "v2board:device-sync",
                "RedisNetwork": "tcp",
                "RedisAddr": "127.0.0.1:6379",
                "RedisDB": 0,
                "Timeout": 2,
                "Expiry": 120,
                "RefreshInterval": 40,
                "MaxIPsPerUser": 256,
                "KeyPrefix": "znode:device",
                "FailClosed": false
            }
        }
    ]
}
EOF
        chmod 600 /etc/znode/config.json
        echo -e "${green}Đã tạo xong tệp cấu hình znode, đang khởi động lại dịch vụ.${plain}"
        if [[ x"${release}" == x"alpine" ]]; then
            service znode restart
        else
            systemctl restart znode
        fi
        sleep 2
        check_status
        echo -e ""
        if [[ $? == 0 ]]; then
            echo -e "${green}Khởi động lại znode thành công${plain}"
        else
            echo -e "${red}Có thể znode khởi động thất bại. Hãy dùng znode log để xem nhật ký.${plain}"
        fi
}

migrate_legacy_v2node() {
    if [[ ! -f /etc/v2node/config.json || -f /etc/znode/config.json ]]; then
        return 0
    fi

    echo -e "${yellow}Phát hiện cấu hình v2node cũ, đang chuyển sang ZNode...${plain}"
    mkdir -p /etc/znode
    cp -p /etc/v2node/config.json /etc/znode/config.json
    chmod 600 /etc/znode/config.json
    [[ -f /etc/v2node/release-repo ]] && cp -p /etc/v2node/release-repo /etc/znode/release-repo
    [[ -f /etc/v2node/release-branch ]] && cp -p /etc/v2node/release-branch /etc/znode/release-branch

    if [[ x"${release}" == x"alpine" ]]; then
        service v2node stop >/dev/null 2>&1 || true
        rc-update del v2node >/dev/null 2>&1 || true
    else
        systemctl stop v2node >/dev/null 2>&1 || true
        systemctl disable v2node >/dev/null 2>&1 || true
    fi
    echo -e "${green}Đã giữ nguyên cấu hình và danh tính agent trong /etc/znode/config.json.${plain}"
}

generate_znode_agent_config() {
        local api_host="$1"
        local agent_id="$2"
        local agent_token="$3"
	local poll_interval="$4"
	local agent_instance_id
	agent_instance_id=$(cat /proc/sys/kernel/random/uuid 2>/dev/null || true)
	if [[ -z "$agent_instance_id" ]]; then
		agent_instance_id="$(hostname)-$(date +%s)"
	fi

        mkdir -p /etc/znode >/dev/null 2>&1
        cat > /etc/znode/config.json <<EOF
{
    "Log": {
        "Level": "warning",
        "Output": "",
        "Access": "none"
    },
    "ConnectionConfig": {
        "Handshake": 4,
        "ConnIdle": 30,
        "UplinkOnly": 2,
        "DownlinkOnly": 4,
        "BufferSize": 16,
        "DisableUDPContentSniffing": true
    },
    "Agent": {
        "Enable": true,
        "ApiHost": "${api_host}",
        "AgentID": "${agent_id}",
		"AgentInstanceID": "${agent_instance_id}",
        "AgentToken": "${agent_token}",
        "PollInterval": ${poll_interval},
        "GlobalDeviceLimitConfig": {
            "Enable": true,
            "SyncEnabled": true,
            "SyncChannel": "v2board:device-sync",
            "RedisNetwork": "tcp",
            "RedisAddr": "127.0.0.1:6379",
            "RedisDB": 0,
            "Timeout": 2,
            "Expiry": 120,
            "RefreshInterval": 40,
            "MaxIPsPerUser": 256,
            "KeyPrefix": "znode:device",
            "FailClosed": false
        }
    },
    "Nodes": []
}
EOF
        chmod 600 /etc/znode/config.json
        echo -e "${green}Znode agent config generated; assigned nodes will be synchronized automatically.${plain}"
        if [[ x"${release}" == x"alpine" ]]; then
            service znode restart
        else
            systemctl restart znode
        fi
        sleep 2
        check_status
        if [[ $? == 0 ]]; then
            echo -e "${green}znode agent started successfully.${plain}"
        else
            echo -e "${red}znode agent may have failed to start; run: znode log${plain}"
        fi
}

install_znode() {
    local version_param="$1"
    if [[ -e /usr/local/znode/ ]]; then
        # Keep one known-good binary for an instant rollback after an update.
        # Configuration lives in /etc/znode and is intentionally not copied.
        rm -rf /usr/local/znode.previous/
        mkdir -p /usr/local/znode.previous/
        cp -a /usr/local/znode/. /usr/local/znode.previous/
        rm -rf /usr/local/znode/
    fi

    mkdir /usr/local/znode/ -p
    cd /usr/local/znode/

    if  [[ -z "$version_param" ]] ; then
        last_version=$(curl -Ls "https://api.github.com/repos/${RELEASE_REPO_ARG}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
        if [[ ! -n "$last_version" ]]; then
            echo -e "${red}Không xác định được phiên bản znode, có thể đã vượt giới hạn GitHub API. Hãy thử lại sau hoặc chỉ định phiên bản để cài thủ công.${plain}"
            exit 1
        fi
        echo -e "${green}Đã tìm thấy phiên bản mới nhất: ${last_version}. Bắt đầu cài đặt...${plain}"
        url="https://github.com/${RELEASE_REPO_ARG}/releases/download/${last_version}/znode-linux-${arch}.zip"
        curl -sL "$url" | pv -s 30M -W -N "Tiến trình tải" > /usr/local/znode/znode-linux.zip
        if [[ $? -ne 0 ]]; then
            echo -e "${red}Tải znode thất bại. Hãy đảm bảo máy chủ có thể tải tệp từ GitHub.${plain}"
            exit 1
        fi
    else
    last_version=$version_param
        url="https://github.com/${RELEASE_REPO_ARG}/releases/download/${last_version}/znode-linux-${arch}.zip"
        curl -sL "$url" | pv -s 30M -W -N "Tiến trình tải" > /usr/local/znode/znode-linux.zip
        if [[ $? -ne 0 ]]; then
            echo -e "${red}Tải znode $1 thất bại. Hãy kiểm tra phiên bản này có tồn tại.${plain}"
            exit 1
        fi
    fi

    unzip znode-linux.zip
    rm znode-linux.zip -f
    chmod +x znode
    mkdir /etc/znode/ -p
    if ! install_geodata; then
        echo -e "${red}Cài dữ liệu GeoIP/GeoSite thất bại; dừng để tránh chạy rule định tuyến sai.${plain}"
        exit 1
    fi
    if [[ x"${release}" == x"alpine" ]]; then
        rm /etc/init.d/znode -f
        cat <<EOF > /etc/init.d/znode
#!/sbin/openrc-run

name="znode"
description="znode"

command="/usr/local/znode/znode"
command_args="server"
command_user="root"
export XRAY_LOCATION_ASSET="/etc/znode"

pidfile="/run/znode.pid"
command_background="yes"

depend() {
        need net
}
EOF
        chmod +x /etc/init.d/znode
        rc-update add znode default
        echo -e "${green}Đã cài znode ${last_version}${plain} và bật tự khởi động cùng hệ thống."
    else
        rm /etc/systemd/system/znode.service -f
        cat <<EOF > /etc/systemd/system/znode.service
[Unit]
Description=znode Service
After=network.target nss-lookup.target
Wants=network.target

[Service]
User=root
Group=root
Type=simple
Environment=XRAY_LOCATION_ASSET=/etc/znode
LimitAS=infinity
LimitRSS=infinity
LimitCORE=infinity
LimitNOFILE=999999
WorkingDirectory=/usr/local/znode/
ExecStart=/usr/local/znode/znode server
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        systemctl stop znode
        systemctl enable znode
        echo -e "${green}Đã cài znode ${last_version}${plain} và bật tự khởi động cùng hệ thống."
    fi

    if [[ ! -f /etc/znode/config.json ]]; then
        # Nếu CLI đã truyền đủ tham số thì tạo cấu hình trực tiếp và bỏ qua bước hỏi
        if [[ -n "$API_HOST_ARG" && -n "$AGENT_ID_ARG" && -n "$AGENT_TOKEN_ARG" ]]; then
            generate_znode_agent_config "$API_HOST_ARG" "$AGENT_ID_ARG" "$AGENT_TOKEN_ARG" "$POLL_INTERVAL_ARG"
            echo -e "${green}Agent config written to /etc/znode/config.json${plain}"
            first_install=false
        elif [[ -n "$API_HOST_ARG" && -n "$NODE_ID_ARG" && -n "$API_KEY_ARG" ]]; then
            generate_znode_config "$API_HOST_ARG" "$NODE_ID_ARG" "$API_KEY_ARG"
            echo -e "${green}Đã tạo /etc/znode/config.json từ các tham số được cung cấp.${plain}"
            first_install=false
        else
            cp config.json /etc/znode/
            first_install=true
        fi
    else
        if [[ x"${release}" == x"alpine" ]]; then
            service znode start
        else
            systemctl start znode
        fi
        sleep 2
        check_status
        echo -e ""
        if [[ $? == 0 ]]; then
            echo -e "${green}Khởi động lại znode thành công${plain}"
        else
            echo -e "${red}Có thể znode khởi động thất bại. Hãy dùng znode log để xem nhật ký.${plain}"
        fi
        first_install=false
    fi


    printf '%s\n' "$RELEASE_REPO_ARG" > /etc/znode/release-repo
    printf '%s\n' "$RELEASE_BRANCH_ARG" > /etc/znode/release-branch
    chmod 644 /etc/znode/release-repo /etc/znode/release-branch
    curl -o /usr/bin/znode -Ls "https://raw.githubusercontent.com/${RELEASE_REPO_ARG}/${RELEASE_BRANCH_ARG}/script/znode.sh"
    chmod +x /usr/bin/znode

    cd $cur_dir
    rm -f install.sh
    echo "------------------------------------------"
    echo -e "Cách sử dụng script quản lý: "
    echo "------------------------------------------"
    echo "znode              - Hiển thị menu quản lý (đầy đủ chức năng)"
    echo "znode start        - Khởi động znode"
    echo "znode stop         - Dừng znode"
    echo "znode restart      - Khởi động lại znode"
    echo "znode status       - Xem trạng thái znode"
    echo "znode enable       - Bật tự khởi động cùng hệ thống"
    echo "znode disable      - Tắt tự khởi động cùng hệ thống"
    echo "znode log          - Xem nhật ký znode"
    echo "znode generate     - Tạo tệp cấu hình znode"
    echo "znode update       - Cập nhật znode"
    echo "znode update x.x.x - Cập nhật znode lên phiên bản chỉ định"
    echo "znode rollback     - Quay lại bản znode trước đó"
    echo "znode install      - Cài đặt znode"
    echo "znode uninstall    - Gỡ cài đặt znode"
    echo "znode version      - Xem phiên bản znode"
    echo "------------------------------------------"
    if [[ $first_install == true ]]; then
        read -rp "Đây là lần đầu cài znode. Bạn có muốn tự động tạo /etc/znode/config.json không? (y/n): " if_generate
        if [[ "$if_generate" =~ ^[Yy]$ ]]; then
            # Thu thập tham số tương tác và cung cấp giá trị mặc định mẫu
            read -rp "Địa chỉ API panel [định dạng: https://example.com/]: " api_host
            api_host=${api_host:-https://example.com/}
            read -rp "ID node: " node_id
            node_id=${node_id:-1}
            read -rp "Khóa kết nối node: " api_key

            # Tạo tệp cấu hình và ghi đè mẫu có thể đã được sao chép từ gói cài đặt
            generate_znode_config "$api_host" "$node_id" "$api_key"
        else
            echo "${green}Đã bỏ qua bước tự tạo cấu hình. Có thể chạy znode generate để tạo sau.${plain}"
        fi
    fi
}

parse_args "$@"
validate_release_source
validate_agent_args
if declare -F migrate_legacy_v2node >/dev/null 2>&1; then
    migrate_legacy_v2node
fi
validate_existing_agent_binding
echo -e "${green}Bắt đầu cài đặt${plain}"
install_base
install_znode "$VERSION_ARG"

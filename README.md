# ZNode Distribution

Repository phân phối chính thức của ZNode.

Repository này chỉ chứa script cài đặt/quản lý và các binary đã biên dịch trong
[GitHub Releases](../../releases). Mã nguồn không được phân phối tại đây.

## Cài đặt agent

```bash
wget -O znode-install.sh https://raw.githubusercontent.com/AZZ-vopp/znode/main/script/install.sh
bash znode-install.sh --api-host 'https://your-panel.example.com' --agent-id 'AGENT_ID' --agent-token 'AGENT_TOKEN' --release-repo 'AZZ-vopp/znode' --release-branch 'main'
```

Hãy sử dụng lệnh riêng được tạo trong trang quản trị ZBoard để điền đúng thông
tin agent. Không chia sẻ agent token.

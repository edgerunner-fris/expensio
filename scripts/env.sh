#!/usr/bin/env bash
# 从 .env.example 生成 .env，并为本地开发填入随机密钥。
#
# 为什么不在模板里写死"local_only"口令：
#   仓库是公开的，固定口令意味着任何拿到代码的人都能连上正在运行的本地实例；
#   更糟的是这种值经常被原样带进预发环境。这里每次生成新值，.env 在 .gitignore 中。
#
# 已存在 .env 时不覆盖，只补齐模板里新增的键：数据库卷里的角色口令是在首次
# 初始化时写死的，换掉 .env 里的口令并不会改数据库里的角色，只会让连接失败。
# 要轮换口令必须 ALTER ROLE，或删掉 pgdata 卷后重新初始化。
set -euo pipefail

cd "$(dirname "$0")/.."

rand_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "${1:-32}"
  else
    # coreutils 的 od 在所有目标平台都有；去掉空格和换行即得十六进制串。
    od -An -N"${1:-32}" -tx1 /dev/urandom | tr -d ' \n'
  fi
}

# 生成 Ed25519 密钥对，输出 "<base64 种子> <base64 公钥>"。
# Ed25519 的 PKCS#8 私钥固定 48 字节、SPKI 公钥固定 44 字节，
# 末尾 32 字节分别是种子与公钥，前面是固定前缀——所以可以直接截取。
gen_ed25519() {
  if ! command -v openssl >/dev/null 2>&1; then
    echo "生成 Ed25519 密钥需要 openssl；请安装后重试，或手工填写 EXPENSIO_JWT_PRIVATE_KEY / EXPENSIO_JWT_PUBLIC_KEYS。" >&2
    return 1
  fi
  keyfile=$(mktemp)
  openssl genpkey -algorithm ED25519 -out "$keyfile" 2>/dev/null
  seed=$(openssl pkey -in "$keyfile" -outform DER | tail -c 32 | base64 | tr -d '\n')
  pub=$(openssl pkey -in "$keyfile" -pubout -outform DER | tail -c 32 | base64 | tr -d '\n')
  rm -f "$keyfile"
  if [ -z "$seed" ] || [ -z "$pub" ]; then
    echo "生成 Ed25519 密钥失败（openssl 输出为空）。" >&2
    return 1
  fi
  printf '%s %s\n' "$seed" "$pub"
}

set_kv() {
  key=$1
  value=$2
  tmp=$(mktemp)
  awk -v k="$key" -v v="$value" '
    BEGIN { FS = "=" }
    $1 == k { print k "=" v; next }
    { print }
  ' .env >"$tmp"
  mv "$tmp" .env
}

if [ -f .env ]; then
  echo ".env 已存在：只补齐模板中缺失的键，不覆盖现有值。"
  added=""
  while IFS= read -r line; do
    key=${line%%=*}
    case "$key" in
      "" | \#*) continue ;;
    esac
    if ! grep -q "^${key}=" .env; then
      printf '%s\n' "$line" >>.env
      added="$added $key"
    fi
  done <.env.example

  # 新补进来的服务间令牌不能沿用模板占位值；已有值保持不动，
  # 否则 identity 与 notification 会在滚动重启期间出现令牌不一致。
  case " $added " in
    *" EXPENSIO_NOTIFICATION_INTERNAL_TOKEN "*)
      set_kv EXPENSIO_NOTIFICATION_INTERNAL_TOKEN "$(rand_hex)"
      echo "已为新补的 EXPENSIO_NOTIFICATION_INTERNAL_TOKEN 生成随机值。"
      ;;
  esac

  # 升级到 EdDSA 时新模板会多出两个空键。只有两半都缺才生成新密钥对；
  # 只补到一半说明有人手工改过 .env，直接覆盖私钥会让已签出的令牌失效。
  ed_added=0
  case " $added " in
    *" EXPENSIO_JWT_PRIVATE_KEY "*) ed_added=$((ed_added + 1)) ;;
  esac
  case " $added " in
    *" EXPENSIO_JWT_PUBLIC_KEYS "*) ed_added=$((ed_added + 1)) ;;
  esac
  if [ "$ed_added" -eq 2 ]; then
    read -r ed_priv ed_pub <<<"$(gen_ed25519)"
    set_kv EXPENSIO_JWT_PRIVATE_KEY "$ed_priv"
    set_kv EXPENSIO_JWT_PUBLIC_KEYS "$ed_pub"
    echo "已为新补的 JWT Ed25519 密钥对生成随机值。"
  elif [ "$ed_added" -eq 1 ]; then
    echo "警告：JWT 密钥对只补齐了一半，请检查 .env 中的 EXPENSIO_JWT_PRIVATE_KEY / EXPENSIO_JWT_PUBLIC_KEYS。" >&2
  fi

  if [ -n "$added" ]; then
    echo "补齐的键：$added"
  else
    echo "没有缺失的键。"
  fi
  exit 0
fi

cp .env.example .env

db_app=$(rand_hex)
db_migrator=$(rand_hex)
postgres_pw=$(rand_hex)
jwt_secret=$(rand_hex)
read -r jwt_private jwt_public <<<"$(gen_ed25519)"
billing_secret=$(rand_hex)
internal_token=$(rand_hex)
minio_secret=$(rand_hex)
grafana_pw=$(rand_hex 16)

# 宿主进程用 EXPENSIO_DB_PASSWORD，容器初始化用 EXPENSIO_DB_APP_PASSWORD；
# 两者必须相同，否则 make run-<service> 与 app profile 会连不上同一个角色。
set_kv EXPENSIO_DB_PASSWORD "$db_app"
set_kv EXPENSIO_DB_APP_PASSWORD "$db_app"
set_kv EXPENSIO_DB_MIGRATOR_PASSWORD "$db_migrator"
set_kv POSTGRES_PASSWORD "$postgres_pw"
set_kv EXPENSIO_JWT_SECRET "$jwt_secret"
set_kv EXPENSIO_JWT_PRIVATE_KEY "$jwt_private"
set_kv EXPENSIO_JWT_PUBLIC_KEYS "$jwt_public"
set_kv EXPENSIO_BILLING_WEBHOOK_SECRET "$billing_secret"
set_kv EXPENSIO_NOTIFICATION_INTERNAL_TOKEN "$internal_token"
set_kv EXPENSIO_MINIO_SECRET_KEY "$minio_secret"
set_kv MINIO_ROOT_PASSWORD "$minio_secret"
set_kv GRAFANA_ADMIN_PASSWORD "$grafana_pw"

chmod 600 .env

echo "已生成 .env（权限 600），所有本地密钥均为新生成的随机值。"
echo "迁移口令只含 [0-9a-f]，可以安全拼进 postgres:// 连接串。"

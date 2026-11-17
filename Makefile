SERVICES := services/api services/bff services/auth
COMPOSE  := docker compose
KC       := docker compose -f compose.yaml -f compose.keycloak.yaml

# E2E はリフレッシュの動きを短時間で見るため、トークンの期限を短くして起動する
E2E_ENV := ACCESS_TOKEN_TTL=15s REFRESH_SKEW=5s

.PHONY: help setup up down restart logs ps fmt fmt-check vet test check-sensitive compose-check \
        lint verify e2e e2e-up up-keycloak down-keycloak e2e-keycloak scale clean

help: ## コマンド一覧
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  %-16s %s\n", $$1, $$2}'

setup: ## 初回: .env（ランダムな秘密）・ローカル証明書・git フック
	@./scripts/setup.sh

up: ## 起動（自作 OIDC 構成）
	$(COMPOSE) up -d --build --wait

down: ## 停止（データは残す）
	$(COMPOSE) down --remove-orphans

clean: ## 停止してボリュームも消す
	$(COMPOSE) down -v --remove-orphans

logs: ## ログを追う
	$(COMPOSE) logs -f

ps:
	$(COMPOSE) ps

fmt: ## gofmt で整形
	@gofmt -w $(SERVICES)

fmt-check:
	@out="$$(gofmt -l $(SERVICES))"; if [ -n "$$out" ]; then echo "gofmt が必要: $$out"; exit 1; fi; echo "[fmt] OK"

vet:
	@for s in $(SERVICES); do (cd $$s && go vet ./...) || exit 1; done; echo "[vet] OK"

test: ## 単体テスト
	@for s in $(SERVICES); do (cd $$s && go test -race -count=1 ./...) || exit 1; done

check-sensitive:
	@./scripts/check-sensitive.sh --all

compose-check:
	@$(COMPOSE) config --quiet && { [ ! -f compose.keycloak.yaml ] || $(KC) config --quiet; } && echo "[compose] OK"

lint: fmt-check vet check-sensitive compose-check

e2e-up:
	$(E2E_ENV) $(COMPOSE) up -d --build --wait

e2e: ## E2E（起動中のスタックに対して。make e2e-up で短い期限で起動してから）
	@./scripts/e2e.sh

verify: lint test e2e-up e2e ## 完了の定義: 静的検査＋単体テスト＋起動して E2E

scale: ## BFF を3台に増やす（Redis でセッションを共有できているかの確認用）
	$(COMPOSE) up -d --no-recreate --scale bff=3 --wait

up-keycloak: ## 認証基盤を Keycloak に差し替えて起動
	$(KC) up -d --build --wait

down-keycloak:
	$(KC) down --remove-orphans

e2e-keycloak: ## Keycloak 構成での E2E（ログイン〜API〜ログアウト）
	@IDP=keycloak ./scripts/e2e.sh

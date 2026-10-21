# morula 的构建与本地发布。
#   make dev       直接运行（不走 .app，不能自更新）
#   make install   首次安装：打包并放到 /Applications/morula.app
#   make release   递增补丁版本，打包并发布到本地通道，已安装的客户端会提示更新
#   make package   打给别人用的安装包 dist/morula-<版本>.dmg：Apple 芯片和 Intel 通用，不带本机的更新通道

VERSION  := $(shell cat VERSION)
CHANNEL  := $(CURDIR)/dist/channel
APP_DIR  := /Applications
LDFLAGS   = -s -w -X main.version=$(VERSION) -X morula/internal/update.DefaultFeed=$(CHANNEL)/latest.json
export CGO_CFLAGS  = -mmacosx-version-min=13.0
export CGO_LDFLAGS = -mmacosx-version-min=13.0

.PHONY: build app dev install release bump clean package

build:
	go build -tags production -trimpath -ldflags="$(LDFLAGS)" -o dist/morula .

app: build
	@rm -rf dist/morula.app
	@mkdir -p dist/morula.app/Contents/MacOS dist/morula.app/Contents/Resources
	@cp dist/morula dist/morula.app/Contents/MacOS/morula
	@sed 's/@VERSION@/$(VERSION)/' build/darwin/Info.plist > dist/morula.app/Contents/Info.plist
	@cp build/darwin/icon/morula.icns dist/morula.app/Contents/Resources/morula.icns
	@# 临时签名的默认要求是 cdhash，每次构建都变，macOS 把新版当成另一个应用，访问文件的授权要重新点一遍。
	@# 要求写成只认 bundle id，装新版后授权还在。
	@codesign --force --deep --sign - -i local.morula.app -r='designated => identifier "local.morula.app"' dist/morula.app
	@echo "  dist/morula.app $(VERSION)"

# 别人的电脑上没有本机的发布目录，所以不写更新通道，界面不提示更新。
# 临时签名，没有公证：对方第一次打开要到「系统设置 → 隐私与安全性」点「仍要打开」，见 build/darwin/打开方法.txt。
PKG_LDFLAGS = -s -w -X main.version=$(VERSION)
package:
	@rm -rf dist/pkg && mkdir -p dist/pkg/morula.app/Contents/MacOS dist/pkg/morula.app/Contents/Resources
	GOARCH=arm64 go build -tags production -trimpath -ldflags="$(PKG_LDFLAGS)" -o dist/pkg/morula-arm64 .
	CGO_ENABLED=1 GOARCH=amd64 CC="clang -arch x86_64" go build -tags production -trimpath -ldflags="$(PKG_LDFLAGS)" -o dist/pkg/morula-amd64 .
	@lipo -create -output dist/pkg/morula.app/Contents/MacOS/morula dist/pkg/morula-arm64 dist/pkg/morula-amd64
	@sed 's/@VERSION@/$(VERSION)/' build/darwin/Info.plist > dist/pkg/morula.app/Contents/Info.plist
	@cp build/darwin/icon/morula.icns dist/pkg/morula.app/Contents/Resources/morula.icns
	@codesign --force --deep --sign - -i local.morula.app -r='designated => identifier "local.morula.app"' dist/pkg/morula.app
	@mkdir -p dist/pkg/dmg && ditto dist/pkg/morula.app dist/pkg/dmg/morula.app
	@ln -s /Applications dist/pkg/dmg/Applications
	@cp build/darwin/打开方法.txt dist/pkg/dmg/
	@rm -f dist/morula-$(VERSION).dmg
	@hdiutil create -quiet -volname "morula $(VERSION)" -srcfolder dist/pkg/dmg -fs HFS+ -format UDZO dist/morula-$(VERSION).dmg
	@rm -rf dist/pkg
	@echo "  dist/morula-$(VERSION).dmg"

dev:
	go run -ldflags="$(LDFLAGS)" .

install: app
	@rm -rf $(APP_DIR)/morula.app
	@ditto dist/morula.app $(APP_DIR)/morula.app
	@echo "  已安装 $(APP_DIR)/morula.app $(VERSION)"

# 补丁位 +1，例如 0.1.3 → 0.1.4
bump:
	@awk -F. '{printf "%d.%d.%d\n", $$1, $$2, $$3+1}' VERSION > VERSION.tmp && mv VERSION.tmp VERSION

release: bump
	@$(MAKE) --no-print-directory app
	@mkdir -p $(CHANNEL)
	@v=$$(cat VERSION); zip=morula-$$v.zip; \
	  rm -f $(CHANNEL)/$$zip; \
	  ditto -c -k --keepParent dist/morula.app $(CHANNEL)/$$zip; \
	  sum=$$(shasum -a 256 $(CHANNEL)/$$zip | cut -d' ' -f1); \
	  printf '{\n  "version": "%s",\n  "notes": "%s",\n  "zip": "%s",\n  "sha256": "%s"\n}\n' "$$v" "$(NOTES)" "$$zip" "$$sum" > $(CHANNEL)/latest.json.tmp; \
	  mv $(CHANNEL)/latest.json.tmp $(CHANNEL)/latest.json; \
	  echo "  已发布 $$v 到 $(CHANNEL)"

clean:
	rm -rf dist

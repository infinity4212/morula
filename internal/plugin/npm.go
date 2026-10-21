package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// 插件页「发现」：在 npm 上按关键字搜插件，按周下载量排。不内置插件目录，也不依赖别人维护的插件列表。
// 没填关键字时搜 opencode auth，只留不带 @scope/ 的 opencode-<名字>-auth 包，大多是 OpenCode 格式的认证插件；
// 带 @scope/ 的包要填关键字才搜得到，精选里的另在「精选订阅」列出。

const registry = "https://registry.npmjs.org"

// DefaultQuery 是没填关键字时搜的词。
const DefaultQuery = "opencode auth"

var authName = regexp.MustCompile(`^opencode-.+-auth$`)

// Pkg 是 npm 上搜到的一个包。
type Pkg struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Homepage    string `json:"homepage"`
	Publisher   string `json:"publisher"`
	Updated     string `json:"updated"`
	Weekly      int    `json:"weekly"` // 周下载量
}

var npmClient = &http.Client{Timeout: 20 * time.Second}

var searchCache struct {
	sync.Mutex
	q   string
	at  time.Time
	out []Pkg
}

// Search 在 npm 上搜包，按周下载量从多到少排，最多 60 个。同一个关键字 10 分钟内用缓存。
func Search(ctx context.Context, q string) ([]Pkg, error) {
	q = strings.TrimSpace(q)
	def := q == ""
	if def {
		q = DefaultQuery
	}
	searchCache.Lock()
	if searchCache.q == q && time.Since(searchCache.at) < 10*time.Minute {
		out := slices.Clone(searchCache.out)
		searchCache.Unlock()
		return out, nil
	}
	searchCache.Unlock()
	var res struct {
		Objects []struct {
			Package struct {
				Name        string `json:"name"`
				Version     string `json:"version"`
				Description string `json:"description"`
				Date        string `json:"date"`
				Links       struct {
					Homepage string `json:"homepage"`
					NPM      string `json:"npm"`
				} `json:"links"`
				Publisher struct {
					Username string `json:"username"`
				} `json:"publisher"`
			} `json:"package"`
			Downloads struct {
				Weekly int `json:"weekly"`
			} `json:"downloads"`
			Updated string `json:"updated"`
		} `json:"objects"`
	}
	if err := getJSON(ctx, registry+"/-/v1/search?size=100&text="+url.QueryEscape(q), &res); err != nil {
		return nil, fmt.Errorf("搜 npm 失败：%w", err)
	}
	out := []Pkg{}
	for _, o := range res.Objects {
		p := o.Package
		if def && !authName.MatchString(p.Name) {
			continue
		}
		home := p.Links.Homepage
		if home == "" {
			home = p.Links.NPM
		}
		up := o.Updated
		if up == "" {
			up = p.Date
		}
		out = append(out, Pkg{Name: p.Name, Version: p.Version, Description: p.Description, Homepage: home, Publisher: p.Publisher.Username, Updated: up, Weekly: o.Downloads.Weekly})
	}
	slices.SortStableFunc(out, func(a, b Pkg) int { return b.Weekly - a.Weekly })
	if len(out) > 60 {
		out = out[:60]
	}
	searchCache.Lock()
	searchCache.q, searchCache.at, searchCache.out = q, time.Now(), slices.Clone(out)
	searchCache.Unlock()
	return out, nil
}

// Latest 查一个包在 npm 上的最新版本。
func Latest(ctx context.Context, name string) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := getJSON(ctx, registry+"/"+strings.Replace(url.PathEscape(name), "%40", "@", 1)+"/latest", &v); err != nil {
		return "", err
	}
	if v.Version == "" {
		return "", errors.New("npm 上没有 " + name + " 的版本")
	}
	return v.Version, nil
}

// Updates 对每个从 npm 装的插件查最新版本，返回有新版本的：包名 → 新版本。本地文件夹装的不查。
// 查不到的包记在 errs 里，不影响别的。
func Updates(ctx context.Context) (map[string]string, map[string]string, error) {
	list, err := List()
	if err != nil {
		return nil, nil, err
	}
	out, errs := map[string]string{}, map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, e := range list {
		if local(e.Spec) {
			continue
		}
		wg.Add(1)
		go func(e Entry) {
			defer wg.Done()
			v, err := Latest(ctx, e.Name)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[e.Name] = err.Error()
			} else if cur := Meta(e.Name).Version; cur != "" && cur != v {
				out[e.Name] = v
			}
		}(e)
	}
	wg.Wait()
	return out, errs, nil
}

// local 判断安装写法是不是本地文件夹。
func local(spec string) bool {
	return strings.HasPrefix(spec, "/") || strings.HasPrefix(spec, "~") || strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "file:")
}

func getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := npmClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

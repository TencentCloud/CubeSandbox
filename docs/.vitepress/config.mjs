import { readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'
import llmstxt from 'vitepress-plugin-llms'
import {
  crawlerHeadTags,
  regroupLlmsTxtByLanguage,
  reorderLlmsFullByLanguage,
  shouldDropFromSitemap,
  SITE_ORIGIN
} from './seo.mjs'

export default withMermaid(defineConfig({
  title: "CubeSandbox",
  description: "Instant, Concurrent, Secure & Lightweight Sandbox Service for AI Agents",
  srcExclude: ['**/_template.md'],
  cleanUrls: true,

  sitemap: {
    hostname: SITE_ORIGIN,
    transformItems(items) {
      return items.filter((item) => !shouldDropFromSitemap(item.url))
    }
  },

  vite: {
    plugins: [
      ...llmstxt({
        domain: SITE_ORIGIN,
        title: 'CubeSandbox',
        generateLLMsTxt: true,
        generateLLMsFullTxt: true,
        generateLLMFriendlyDocsForEachPage: true,
        stripHTML: true,
        injectLLMHint: false,
        excludeUnnecessaryFiles: false,
        excludeIndexPage: false,
        excludeBlog: false,
        ignoreFiles: [
          '**/_template.md'
        ]
      })
    ]
  },

  transformHead({ pageData }) {
    return crawlerHeadTags({ relativePath: pageData.relativePath })
  },

  buildEnd(siteConfig) {
    const llmsTxt = join(siteConfig.outDir, 'llms.txt')
    writeFileSync(llmsTxt, regroupLlmsTxtByLanguage(readFileSync(llmsTxt, 'utf8')))
    const llmsFull = join(siteConfig.outDir, 'llms-full.txt')
    writeFileSync(llmsFull, reorderLlmsFullByLanguage(readFileSync(llmsFull, 'utf8')))
  },

  markdown: {
    // Shiki has no bundled PromQL grammar; alias so ```promql blocks do not warn.
    languageAlias: {
      promql: 'js'
    }
  },
  
  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: '/favicon.svg' }]
  ],
  
  themeConfig: {
    outline: { level: [1, 3] },
    logo: '/logo.svg',
    socialLinks: [
      { icon: 'github', link: 'https://github.com/tencentcloud/CubeSandbox' }
    ],
    search: {
      provider: 'local',
      options: {
        miniSearch: {
          options: {
            tokenize: (text) => text
              ? text.split(/([\u4e00-\u9fa5])|[\s\W]+/).filter(Boolean)
              : []
          },
          searchOptions: {
            fuzzy: 0.2,
            prefix: true,
            boost: { title: 4, text: 2, titles: 1 }
          }
        },
        _render(src, env, md) {
          const html = md.render(src, env)
          if (env.frontmatter?.search === false) return ''
          const fm = env.frontmatter ?? {}
          if (!html.trim()) {
            // No markdown body (e.g. external-link blog posts).
            // Build a synthetic VitePress-style heading so splitPageIntoSections
            // picks it up. The actual page will redirect to externalUrl on load.
            if (!fm.title) return ''
            const slug = fm.title.toLowerCase()
              .replace(/\s+/g, '-')
              .replace(/[^\w-]/g, '')
              .replace(/-+/g, '-')
            return `<h1 id="${slug}" tabindex="-1">${fm.title} <a class="header-anchor" href="#${slug}">\u200B</a></h1>${fm.description ? `<p>${fm.description}</p>` : ''}`
          }
          // Inject frontmatter description right after the first heading so it
          // becomes part of that section's indexed text.
          if (fm.description) {
            return html.replace(/(<\/h[1-6]>)/, `$1<p>${fm.description}</p>`)
          }
          return html
        },
        locales: {
          root: {
            translations: {
              button: { buttonText: 'Search', buttonAriaLabel: 'Search docs' },
              modal: {
                displayDetails: 'Display detailed list',
                resetButtonTitle: 'Reset search',
                backButtonTitle: 'Close search',
                noResultsText: 'No results for',
                footer: {
                  selectText: 'to select',
                  navigateText: 'to navigate',
                  closeText: 'to close'
                }
              }
            }
          },
          zh: {
            translations: {
              button: { buttonText: '搜索', buttonAriaLabel: '搜索文档' },
              modal: {
                displayDetails: '显示详细列表',
                resetButtonTitle: '清空搜索',
                backButtonTitle: '关闭搜索',
                noResultsText: '没有找到相关结果',
                footer: { selectText: '选择', navigateText: '切换', closeText: '关闭' }
              }
            }
          }
        }
      }
    }
  },

  locales: {
    root: {
      label: 'English',
      lang: 'en',
      themeConfig: {
        nav: [
          { text: 'Home', link: '/' },
          { text: 'Guide', link: '/guide/introduction' },
          { text: 'Architecture', link: '/architecture/overview' },
          { text: 'Developer', link: '/dev/' },
          {
            text: 'Community',
            items: [
              { text: 'Roadmap', link: '/guide/roadmap' },
              { text: 'Cube 100 Program', link: '/guide/cube100' },
              { text: 'Contributing', link: 'https://github.com/tencentcloud/CubeSandbox/blob/master/CONTRIBUTING.md' }
            ]
          },
          { text: 'Blog', link: '/blog/' },
          { text: 'Changelog', link: '/changelog/' },
          { text: 'Contributors', link: '/contributors' },
          { text: 'About us', link: '/about-us' },
          { text: 'GitHub', link: 'https://github.com/tencentcloud/CubeSandbox' }
        ],
        sidebar: {
          '/blog/': [],
          '/guide/': [
            {
              text: 'Getting Started',
              items: [
                { text: 'Introduction', link: '/guide/introduction' },
                { text: 'Quick Start', link: '/guide/quickstart' }
              ]
            },
            {
              text: 'Deployment',
              items: [
                { text: 'PVM Deployment', link: '/guide/pvm-deploy' },
                { text: 'Bare-Metal Deployment', link: '/guide/bare-metal-deploy' },
                { text: 'Self-Build Deployment', link: '/guide/self-build-deploy' },
                { text: 'Multi-Node Cluster', link: '/guide/multi-node-deploy' },
                {
                  text: 'Kubernetes Deployment',
                  link: '/guide/kubernetes/',
                  collapsed: true,
                  items: [
                    { text: 'Overview', link: '/guide/kubernetes/' },
                    { text: 'Helm Install', link: '/guide/kubernetes/install' },
                    { text: 'Architecture', link: '/guide/kubernetes/architecture' },
                    { text: 'Upgrade', link: '/guide/kubernetes/upgrade' },
                    { text: 'FAQ', link: '/guide/kubernetes/faq' }
                  ]
                },
                { text: 'Tencent Cloud Cluster (Terraform)', link: '/guide/tencentcloud-terraform-deploy' },
                { text: 'Development Environment (QEMU VM)', link: '/guide/dev-environment' },
                { text: 'Downloads & Releases', link: '/guide/downloads' }
              ]
            },
            {
              text: 'Core Concepts',
              items: [
                { text: 'Sandbox Lifecycle', link: '/guide/lifecycle' },
                { text: 'Templates Overview', link: '/guide/templates' },
                { text: 'Snapshot, Rollback & Clone', link: '/guide/snapshot-rollback-clone' },
                { text: 'Cross-Node Snapshots', link: '/guide/cross-node-snapshot' }
              ]
            },
            {
              text: 'Tutorials',
              items: [
                { text: 'Example Projects', link: '/guide/tutorials/examples' },
                {
                  text: 'SDK',
                  collapsed: true,
                  items: [
                    { text: 'Python', link: '/guide/tutorials/sdk/python' },
                    { text: 'Go', link: '/guide/tutorials/sdk/go' },
                    { text: 'Node.js', link: '/guide/tutorials/sdk/nodejs' }
                  ]
                }
              ]
            },
            {
              text: 'Templates & Images',
              items: [
                { text: 'Create Templates from OCI Image', link: '/guide/tutorials/template-from-image' },
                { text: 'Custom Template Images', link: '/guide/tutorials/bring-your-own-image' },
                { text: 'Commit a Running Sandbox', link: '/guide/tutorials/template-from-sandbox' },
                { text: 'Pre-warm a Template Service', link: '/guide/tutorials/prewarm-template-service' },
                { text: 'Local & Remote Image Practice', link: '/guide/tutorials/template-build-practice' },
                { text: 'Template Inspection & Request Preview', link: '/guide/template-inspection-and-preview' },
                { text: 'Template Aliases', link: '/guide/template-aliases' }
              ]
            },
            {
              text: 'Networking & Security',
              items: [
                { text: 'Network Policy', link: '/guide/network-policy' },
                { text: 'Route-Aware Egress', link: '/guide/route-aware-egress' },
                { text: 'Security Proxy', link: '/guide/security-proxy' },
                { text: 'Restrict Public Access', link: '/guide/restrict-public-access' },
                { text: 'Authentication', link: '/guide/authentication' },
                { text: 'HTTPS & Domain Resolution', link: '/guide/https-and-domain' },
                { text: 'Network Hardening', link: '/guide/network-hardening' }
              ]
            },
            {
              text: 'Storage',
              items: [
                { text: 'S3 Volumes', link: '/guide/s3-volume' },
                { text: 'Persistent Storage (Host Mount)', link: '/guide/persistent-storage' },
                { text: 'Volume Plugin Development', link: '/guide/volume-plugin' }
              ]
            },
            {
              text: 'Management Tools',
              items: [
                { text: 'WebUI Dashboard', link: '/guide/webui' },
                { text: 'CLI Tools', link: '/guide/cli-tools' }
              ]
            },
            {
              text: 'Cluster Operations',
              items: [
                { text: 'Node Operations', link: '/guide/node-operations' },
                { text: 'Service Management & Logs', link: '/guide/service-management' },
                { text: 'CubeMaster Scheduler Configuration', link: '/guide/cubemaster-scheduler-config' },
                { text: 'Soft-delete Purge', link: '/guide/soft-delete-purge' },
                { text: 'Component multi-version', link: '/guide/component-multiversion' }
              ]
            },
            {
              text: 'Observability & Performance',
              items: [
                { text: 'Sandbox Resource Metrics', link: '/guide/resource-metrics' },
                { text: 'Sandbox Logs', link: '/guide/sandbox-logs' },
                { text: 'Performance Benchmark', link: '/guide/performance-benchmark' }
              ]
            },
            {
              text: 'Troubleshooting',
              link: '/guide/troubleshooting/',
              items: [
                { text: 'Overview', link: '/guide/troubleshooting/' },
                { text: 'Deployment', link: '/guide/troubleshooting/deployment' },
                { text: 'Templates', link: '/guide/troubleshooting/templates' },
                { text: 'Network CIDR Conflicts', link: '/guide/troubleshooting/local-network-cidr-conflict' },
                { text: 'Host Mount Permissions', link: '/guide/troubleshooting/host-mount-permissions' },
                { text: 'Component Log Locations', link: '/guide/troubleshooting/component-log-locations' }
              ]
            },
            {
              text: 'Features & Integrations',
              items: [
                { text: 'Digital Assistant (Preview)', link: '/guide/digital-assistant' },
                {
                  text: 'Framework & Tool Integrations',
                  link: '/guide/integrations/',
                  collapsed: true,
                  items: [
                    { text: 'Claude Code', link: '/guide/integrations/claude-code' },
                    { text: 'LangChain', link: '/guide/integrations/langchain' },
                    { text: 'Pi Agent', link: '/guide/integrations/pi-agent' },
                    { text: 'OpenAI Agents SDK', link: '/guide/integrations/openai-agents-sdk' }
                  ]
                },
                {
                  text: 'Case Studies',
                  link: '/guide/usecases/',
                  collapsed: true,
                  items: [
                    { text: 'trpc-agent-go', link: '/guide/usecases/trpc-agent-go' },
                    { text: 'Lexmount AI', link: '/guide/usecases/lexmount-browser-agent' },
                    { text: 'Hermes Agent', link: '/guide/usecases/hermes-agent' },
                    { text: 'Lenovo Cloud Agent', link: '/guide/usecases/lenovo-cloud-agent' },
                    { text: 'Horizon Insights', link: '/guide/usecases/horizon-insights' },
                    { text: 'Guangdong Rising', link: '/guide/usecases/guangdong-rising' },
                    { text: 'unisound', link: '/guide/usecases/unisound-rl-rollout' }
                  ]
                }
              ]
            },
            {
              text: 'Maintainer Docs',
              items: [
                { text: 'Blog Maintenance', link: '/guide/maintainer/blog' }
              ]
            }
          ],
          '/architecture/': [
            {
              text: 'System Design',
              items: [
                { text: 'Architecture Overview', link: '/architecture/overview' },
                { text: 'CubeVS Network Model', link: '/architecture/network' }
              ]
            }
          ],
          '/dev/': [
            {
              text: 'Developer Docs',
              items: [
                { text: 'Overview', link: '/dev/' },
                { text: 'Redis Key Convention', link: '/dev/redis-key-spec' }
              ]
            }
          ]
        }
      }
    },
    zh: {
      label: '简体中文',
      lang: 'zh',
      link: '/zh/',
      title: 'CubeSandbox',
      description: '一个极速启动、高并发、安全且轻量化的 AI Agent 沙箱服务',
      themeConfig: {
        nav: [
          { text: '首页', link: '/zh/' },
          { text: '指南', link: '/zh/guide/introduction' },
          { text: '架构', link: '/zh/architecture/overview' },
          { text: '开发者', link: '/zh/dev/' },
          {
            text: '社区',
            items: [
              { text: '路线图', link: '/zh/guide/roadmap' },
              { text: 'Cube 100 计划', link: '/zh/guide/cube100' },
              { text: '参与贡献', link: 'https://github.com/tencentcloud/CubeSandbox/blob/master/CONTRIBUTING_zh.md' }
            ]
          },
          { text: '博客', link: '/zh/blog/' },
          { text: '更新日志', link: '/zh/changelog/' },
          { text: '贡献者', link: '/zh/contributors' },
          { text: '关于我们', link: '/zh/about-us' },
          { text: 'GitHub', link: 'https://github.com/tencentcloud/CubeSandbox' }
        ],
        sidebar: {
          '/zh/blog/': [],
          '/zh/guide/': [
            {
              text: '入门',
              items: [
                { text: '产品简介', link: '/zh/guide/introduction' },
                { text: '快速开始', link: '/zh/guide/quickstart' }
              ]
            },
            {
              text: '部署',
              items: [
                { text: 'PVM 部署', link: '/zh/guide/pvm-deploy' },
                { text: '裸金属部署', link: '/zh/guide/bare-metal-deploy' },
                { text: '本地构建部署', link: '/zh/guide/self-build-deploy' },
                { text: '多机集群部署', link: '/zh/guide/multi-node-deploy' },
                {
                  text: 'Kubernetes 部署',
                  link: '/zh/guide/kubernetes/',
                  collapsed: true,
                  items: [
                    { text: '概览', link: '/zh/guide/kubernetes/' },
                    { text: 'Helm 安装', link: '/zh/guide/kubernetes/install' },
                    { text: '架构说明', link: '/zh/guide/kubernetes/architecture' },
                    { text: '升级', link: '/zh/guide/kubernetes/upgrade' },
                    { text: '常见问题', link: '/zh/guide/kubernetes/faq' }
                  ]
                },
                { text: '腾讯云集群（Terraform）', link: '/zh/guide/tencentcloud-terraform-deploy' },
                { text: '开发环境（QEMU 虚机）', link: '/zh/guide/dev-environment' },
                { text: '下载与 Release 说明', link: '/zh/guide/downloads' }
              ]
            },
            {
              text: '核心概念',
              items: [
                { text: '沙箱生命周期', link: '/zh/guide/lifecycle' },
                { text: '模板概览', link: '/zh/guide/templates' },
                { text: '快照、回滚与克隆', link: '/zh/guide/snapshot-rollback-clone' },
                { text: '跨机快照', link: '/zh/guide/cross-node-snapshot' }
              ]
            },
            {
              text: '教程',
              items: [
                { text: '示例项目', link: '/zh/guide/tutorials/examples' },
                {
                  text: 'SDK',
                  collapsed: true,
                  items: [
                    { text: 'Python', link: '/zh/guide/tutorials/sdk/python' },
                    { text: 'Go', link: '/zh/guide/tutorials/sdk/go' },
                    { text: 'Node.js', link: '/zh/guide/tutorials/sdk/nodejs' }
                  ]
                }
              ]
            },
            {
              text: '模板与镜像',
              items: [
                { text: '从 OCI 镜像制作模板', link: '/zh/guide/tutorials/template-from-image' },
                { text: '自定义模板镜像', link: '/zh/guide/tutorials/bring-your-own-image' },
                { text: '将运行中的沙箱提交为模板', link: '/zh/guide/tutorials/template-from-sandbox' },
                { text: '预热模板服务', link: '/zh/guide/tutorials/prewarm-template-service' },
                { text: '本地与远程镜像实战', link: '/zh/guide/tutorials/template-build-practice' },
                { text: '模板检查与请求预览', link: '/zh/guide/template-inspection-and-preview' },
                { text: '模板别名', link: '/zh/guide/template-aliases' }
              ]
            },
            {
              text: '网络与安全',
              items: [
                { text: '网络策略', link: '/zh/guide/network-policy' },
                { text: '路由感知出网', link: '/zh/guide/route-aware-egress' },
                { text: '安全代理', link: '/zh/guide/security-proxy' },
                { text: '限制公开访问', link: '/zh/guide/restrict-public-access' },
                { text: 'API 鉴权', link: '/zh/guide/authentication' },
                { text: 'HTTPS 与域名解析', link: '/zh/guide/https-and-domain' },
                { text: '网络加固', link: '/zh/guide/network-hardening' }
              ]
            },
            {
              text: '存储',
              items: [
                { text: 'S3 持久卷', link: '/zh/guide/s3-volume' },
                { text: '持久化存储（Host Mount）', link: '/zh/guide/persistent-storage' },
                { text: 'Volume 插件开发', link: '/zh/guide/volume-plugin' }
              ]
            },
            {
              text: '管理工具',
              items: [
                { text: 'WebUI 控制台', link: '/zh/guide/webui' },
                { text: '命令行工具', link: '/zh/guide/cli-tools' }
              ]
            },
            {
              text: '集群运维',
              items: [
                { text: '节点相关操作', link: '/zh/guide/node-operations' },
                { text: '服务管理与日志', link: '/zh/guide/service-management' },
                { text: 'CubeMaster 调度器配置', link: '/zh/guide/cubemaster-scheduler-config' },
                { text: '软删除数据清理', link: '/zh/guide/soft-delete-purge' },
                { text: '组件多版本', link: '/zh/guide/component-multiversion' }
              ]
            },
            {
              text: '可观测性与性能',
              items: [
                { text: '沙箱资源指标', link: '/zh/guide/resource-metrics' },
                { text: '沙箱日志', link: '/zh/guide/sandbox-logs' },
                { text: '性能基准', link: '/zh/guide/performance-benchmark' }
              ]
            },
            {
              text: '故障排查',
              link: '/zh/guide/troubleshooting/',
              items: [
                { text: '排障概览', link: '/zh/guide/troubleshooting/' },
                { text: '部署问题', link: '/zh/guide/troubleshooting/deployment' },
                { text: '模板问题', link: '/zh/guide/troubleshooting/templates' },
                { text: '网络 CIDR 冲突', link: '/zh/guide/troubleshooting/local-network-cidr-conflict' },
                { text: 'Host Mount 权限', link: '/zh/guide/troubleshooting/host-mount-permissions' },
                { text: '组件日志位置', link: '/zh/guide/troubleshooting/component-log-locations' }
              ]
            },
            {
              text: '功能与生态集成',
              items: [
                { text: '数字助手（预览）', link: '/zh/guide/digital-assistant' },
                {
                  text: '框架与工具集成',
                  link: '/zh/guide/integrations/',
                  collapsed: true,
                  items: [
                    { text: 'Claude Code', link: '/zh/guide/integrations/claude-code' },
                    { text: 'LangChain', link: '/zh/guide/integrations/langchain' },
                    { text: 'Pi Agent', link: '/zh/guide/integrations/pi-agent' },
                    { text: 'OpenAI Agents SDK', link: '/zh/guide/integrations/openai-agents-sdk' }
                  ]
                },
                {
                  text: '案例实践',
                  link: '/zh/guide/usecases/',
                  collapsed: true,
                  items: [
                    { text: 'trpc-agent-go', link: '/zh/guide/usecases/trpc-agent-go' },
                    { text: 'Lexmount AI', link: '/zh/guide/usecases/lexmount-browser-agent' },
                    { text: 'Hermes Agent', link: '/zh/guide/usecases/hermes-agent' },
                    { text: 'Lenovo Cloud Agent', link: '/zh/guide/usecases/lenovo-cloud-agent' },
                    { text: 'Horizon Insights', link: '/zh/guide/usecases/horizon-insights' },
                    { text: 'Guangdong Rising', link: '/zh/guide/usecases/guangdong-rising' },
                    { text: 'unisound', link: '/zh/guide/usecases/unisound-rl-rollout' }
                  ]
                }
              ]
            },
            {
              text: '维护者文档',
              items: [
                { text: '博客维护', link: '/zh/guide/maintainer/blog' }
              ]
            }
          ],
          '/zh/architecture/': [
            {
              text: '系统设计',
              items: [
                { text: '架构概览 (Overview)', link: '/zh/architecture/overview' },
                { text: 'CubeVS 网络模型', link: '/zh/architecture/network' }
              ]
            }
          ],
          '/zh/dev/': [
            {
              text: '开发者文档',
              items: [
                { text: '概览', link: '/zh/dev/' },
                { text: 'Redis Key 命名规范', link: '/zh/dev/redis-key-spec' }
              ]
            }
          ]
        }
      }
    }
  }
}))

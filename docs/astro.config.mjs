import { defineConfig } from 'astro/config'
import starlight from '@astrojs/starlight'
import sitemap from '@astrojs/sitemap'
import mermaid from 'astro-mermaid'

// The site is served from a custom domain (public/CNAME), so there is no base
// path: every link below is absolute from the root.
export default defineConfig({
  site: 'https://reactor.robbeverhelst.com',
  integrations: [
    // Must come before starlight: it rewrites ```mermaid fences before the
    // Markdown pipeline turns them into code blocks.
    mermaid({ theme: 'neutral', autoTheme: true }),
    sitemap(),
    starlight({
      title: 'UniFi Reactor',
      description:
        'A Kubernetes operator that turns what your UniFi gear already knows — which WAN is live, whether the UPS is on mains — into declarative actions on your cluster.',
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/robbeverhelst/unifi-reactor',
        },
      ],
      editLink: {
        baseUrl: 'https://github.com/robbeverhelst/unifi-reactor/edit/main/docs/',
      },
      head: [
        {
          // Self-hosted Umami. The website id is a public client identifier, not
          // a credential — but it has to be created in the Umami UI for this
          // site before it counts anything. Until then this is a placeholder and
          // the script reports to a website that does not exist.
          tag: 'script',
          attrs: {
            src: 'https://analytics.robbeverhelst.be/script.js',
            'data-website-id': '9da009e0-9ebf-4a7d-8648-07235ea45669',
            defer: true,
          },
        },
      ],
      sidebar: [
        {
          label: 'Start here',
          items: [
            { label: 'What Reactor is', slug: 'start/what-reactor-is' },
            { label: 'Install', slug: 'start/install' },
            { label: 'Your first Automation', slug: 'start/first-automation' },
          ],
        },
        {
          label: 'Concepts',
          items: [
            { label: 'State, not events', slug: 'concepts/state-not-events' },
            { label: 'Arbitration', slug: 'concepts/arbitration' },
            { label: 'Reversal and baselines', slug: 'concepts/reversal-and-baselines' },
            { label: 'Levels vs occurrences', slug: 'concepts/levels-and-occurrences' },
            { label: 'Settling a noisy signal', slug: 'concepts/settling-a-noisy-signal' },
            { label: 'When Reactor cannot see', slug: 'concepts/when-reactor-cannot-see' },
          ],
        },
        {
          label: 'State keys',
          items: [
            { label: 'The vocabulary', slug: 'state-keys' },
            { label: 'WAN and internet', slug: 'state-keys/wan-and-internet' },
            { label: 'Power and UPS', slug: 'state-keys/power-and-ups' },
            { label: 'Fleet and devices', slug: 'state-keys/fleet-and-devices' },
            { label: 'Outlets', slug: 'state-keys/outlets' },
          ],
        },
        {
          label: 'Actions',
          items: [
            { label: 'Kubernetes', slug: 'actions/kubernetes' },
            { label: 'Notifications and HTTP', slug: 'actions/notifications-and-http' },
            { label: 'External services', slug: 'actions/external-services' },
            { label: 'UniFi console', slug: 'actions/unifi-console' },
          ],
        },
        {
          label: 'Operations',
          items: [
            { label: 'Configuration', slug: 'operations/configuration' },
            { label: 'Suspend and dry run', slug: 'operations/suspend-and-dry-run' },
            { label: 'Metrics, alerts and dashboard', slug: 'operations/metrics-and-alerts' },
            { label: 'Events and status', slug: 'operations/events' },
            { label: 'Webhook fast path', slug: 'operations/webhook-fast-path' },
            { label: 'RBAC and security', slug: 'operations/rbac-and-security' },
            { label: 'Upgrading', slug: 'operations/upgrading' },
            { label: 'Uninstalling', slug: 'operations/uninstalling' },
          ],
        },
        {
          label: 'Troubleshooting',
          items: [
            { label: 'Start here', slug: 'troubleshooting' },
            { label: 'Reading status and Events', slug: 'troubleshooting/reading-status' },
            { label: 'Nothing is happening', slug: 'troubleshooting/nothing-is-happening' },
            { label: 'Credentials and reachability', slug: 'troubleshooting/credentials-and-reachability' },
            { label: 'Missing or wrong state keys', slug: 'troubleshooting/state-keys' },
            { label: 'Actions that did not happen', slug: 'troubleshooting/actions-and-targets' },
            { label: 'Conflicts and drift', slug: 'troubleshooting/conflicts-and-drift' },
            { label: 'RBAC and the CRD', slug: 'troubleshooting/rbac-and-crd' },
            { label: 'Still stuck', slug: 'troubleshooting/still-stuck' },
          ],
        },
        {
          label: 'Contributing',
          items: [
            { label: 'How to contribute', slug: 'contributing' },
            { label: 'Development', slug: 'contributing/development' },
            { label: 'Adding a provider', slug: 'contributing/adding-a-provider' },
            { label: 'Distribution', slug: 'contributing/distribution' },
            { label: 'UniFi Alarm Manager API', slug: 'contributing/unifi-alarm-manager-api' },
            { label: 'Writing to a UniFi console', slug: 'contributing/unifi-write-api' },
          ],
        },
        {
          label: 'Design',
          items: [
            { label: 'Design specification', slug: 'design/spec' },
            { label: 'Stability and roadmap', slug: 'design/stability' },
          ],
        },
      ],
    }),
  ],
})

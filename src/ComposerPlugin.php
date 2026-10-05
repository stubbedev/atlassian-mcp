<?php

declare(strict_types=1);

namespace Stubbedev\AtlassianMcp;

use Composer\Composer;
use Composer\EventDispatcher\EventSubscriberInterface;
use Composer\IO\IOInterface;
use Composer\Plugin\PluginInterface;
use Composer\Script\ScriptEvents;

// On install/update, fetch the native binary for this OS/arch so the MCP client
// can exec it directly: PHP is the installer, not the runtime. If the plugin is
// not allowed (allow-plugins) or the download fails, bin/atlassian-mcp fetches
// the binary on its first run instead.
final class ComposerPlugin implements PluginInterface, EventSubscriberInterface
{
    private IOInterface $io;

    public function activate(Composer $composer, IOInterface $io): void
    {
        $this->io = $io;
    }

    public function deactivate(Composer $composer, IOInterface $io): void {}

    public function uninstall(Composer $composer, IOInterface $io): void {}

    public static function getSubscribedEvents(): array
    {
        return [
            ScriptEvents::POST_INSTALL_CMD => 'fetchBinary',
            ScriptEvents::POST_UPDATE_CMD => 'fetchBinary',
        ];
    }

    public function fetchBinary(): void
    {
        if (getenv('ATLASSIAN_MCP_SKIP_DOWNLOAD') === '1') {
            return;
        }
        try {
            $bin = Binary::ensure();
        } catch (\Throwable $e) {
            $this->io->writeError("<warning>[atlassian-mcp] {$e->getMessage()}</warning>");
            $this->io->writeError('<warning>[atlassian-mcp] The binary will be fetched on first run instead.</warning>');

            return;
        }
        $this->io->write("<info>[atlassian-mcp] Native binary: {$bin}</info>");
    }
}

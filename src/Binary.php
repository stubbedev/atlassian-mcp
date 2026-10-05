<?php

declare(strict_types=1);

namespace Stubbedev\AtlassianMcp;

// Resolves and downloads the prebuilt Go binary for this OS/arch from the
// GitHub release matching package.json's version (the same targets and asset
// names as scripts/download.mjs). Shared by the Composer plugin, which fetches
// it at install time, and bin/atlassian-mcp, which fetches it on first run if
// the plugin was not allowed to.
final class Binary
{
    public const REPO = 'stubbedev/atlassian-mcp';

    // Release targets built by .github/workflows/publish.yml.
    private const SUPPORTED = [
        'linux' => ['amd64', 'arm64', 'arm', '386', 'ppc64le', 's390x', 'riscv64'],
        'darwin' => ['amd64', 'arm64'],
        'windows' => ['amd64', 'arm64', '386'],
        'freebsd' => ['amd64', 'arm64'],
    ];

    private const OS = [
        'Linux' => 'linux',
        'Darwin' => 'darwin',
        'Windows' => 'windows',
        'BSD' => 'freebsd',
    ];

    private const ARCH = [
        'x86_64' => 'amd64',
        'amd64' => 'amd64',
        'aarch64' => 'arm64',
        'arm64' => 'arm64',
        'armv7l' => 'arm',
        'armv7' => 'arm',
        'armv6l' => 'arm',
        'i386' => '386',
        'i686' => '386',
        'x86' => '386',
        'ppc64le' => 'ppc64le',
        's390x' => 's390x',
        'riscv64' => 'riscv64',
    ];

    // Cached inside the package dir (gitignored, like the npm wrapper's copy).
    public static function path(): string
    {
        [, , $ext] = self::target();

        return dirname(__DIR__) . "/bin/atlassian-mcp-native{$ext}";
    }

    // Returns the path to the platform binary, downloading it if not present.
    public static function ensure(): string
    {
        $dest = self::path();
        if (is_file($dest) && filesize($dest) > 0) {
            return $dest;
        }
        self::download(self::assetUrl(), $dest);

        return $dest;
    }

    /** @return array{string, string, string} os, arch, executable extension */
    private static function target(): array
    {
        $machine = strtolower(php_uname('m'));
        $os = self::OS[PHP_OS_FAMILY] ?? null;
        $arch = self::ARCH[$machine] ?? null;
        if ($os === null || $arch === null || !in_array($arch, self::SUPPORTED[$os], true)) {
            throw new \RuntimeException(sprintf(
                'Unsupported platform %s/%s. Build from source with: go install -tags nodynamic github.com/%s@latest',
                PHP_OS_FAMILY,
                $machine,
                self::REPO,
            ));
        }

        return [$os, $arch, $os === 'windows' ? '.exe' : ''];
    }

    private static function assetUrl(): string
    {
        [$os, $arch, $ext] = self::target();

        return sprintf(
            'https://github.com/%s/releases/download/v%s/atlassian-mcp_%s_%s%s',
            self::REPO,
            self::version(),
            $os,
            $arch,
            $ext,
        );
    }

    // package.json is the single source of truth for the version, and the
    // release tag is cut from it, so a pinned package gets that tag's binary.
    private static function version(): string
    {
        $file = dirname(__DIR__) . '/package.json';
        $pkg = json_decode((string) @file_get_contents($file), true);
        if (!is_array($pkg) || !is_string($pkg['version'] ?? null)) {
            throw new \RuntimeException("Cannot read the version from {$file}");
        }

        return $pkg['version'];
    }

    private static function download(string $url, string $dest): void
    {
        $tmp = "{$dest}.download";
        $out = @fopen($tmp, 'wb');
        if ($out === false) {
            throw new \RuntimeException("Cannot write {$tmp}");
        }

        if (function_exists('curl_init')) {
            $ch = curl_init($url);
            curl_setopt_array($ch, [
                CURLOPT_FILE => $out,
                CURLOPT_FOLLOWLOCATION => true,
                CURLOPT_FAILONERROR => true,
                CURLOPT_USERAGENT => 'atlassian-mcp-composer',
                CURLOPT_CONNECTTIMEOUT => 30,
            ]);
            $ok = curl_exec($ch);
            $err = curl_error($ch);
            // No curl_close(): a no-op since PHP 8.0, deprecated as of 8.5.
            unset($ch);
            fclose($out);
            if ($ok === false) {
                @unlink($tmp);
                throw new \RuntimeException("Failed to download {$url}: {$err}");
            }
        } else {
            $ctx = stream_context_create(['http' => [
                'follow_location' => 1,
                'user_agent' => 'atlassian-mcp-composer',
            ]]);
            $in = @fopen($url, 'rb', false, $ctx);
            if ($in === false) {
                fclose($out);
                @unlink($tmp);
                $err = error_get_last()['message'] ?? 'unknown error';
                throw new \RuntimeException("Failed to download {$url}: {$err}");
            }
            stream_copy_to_stream($in, $out);
            fclose($in);
            fclose($out);
        }

        clearstatcache(true, $tmp);
        if (filesize($tmp) === 0) {
            @unlink($tmp);
            throw new \RuntimeException("Failed to download {$url}: empty response");
        }
        @chmod($tmp, 0755);
        @unlink($dest);
        if (!rename($tmp, $dest)) {
            throw new \RuntimeException("Cannot move the binary into place at {$dest}");
        }
    }
}

<?php

declare(strict_types=1);

namespace Stubbedev\AtlassianMcp;

final class Binary
{
    private const NAME = 'atlassian-mcp';
    private const REPO = 'stubbedev/' . self::NAME;
    private const SOURCE_HINT = 'go install -tags nodynamic github.com/' . self::REPO . '@latest';

    private const SUPPORTED = [
        'linux' => ['amd64', 'arm64', 'arm', '386', 'ppc64le', 's390x', 'riscv64'],
        'darwin' => ['arm64'],
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
        'x64' => 'amd64',
        'aarch64' => 'arm64',
        'arm64' => 'arm64',
        'armv7l' => 'arm',
        'armv7' => 'arm',
        'armv6l' => 'arm',
        'i386' => '386',
        'i586' => '386',
        'i686' => '386',
        'x86' => '386',
        'ppc64le' => 'ppc64le',
        's390x' => 's390x',
        'riscv64' => 'riscv64',
    ];

    public static function ensure(): string
    {
        $dest = dirname(__DIR__) . '/bin/' . self::NAME . '-native' . self::target()[2];
        if (self::present($dest)) {
            return $dest;
        }
        $cached = self::cacheDir() . '/' . self::asset();
        if (!self::present($cached)) {
            self::fetch($cached);
        }
        self::place($cached, $dest);

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
                'Unsupported platform %s/%s. Build from source with: %s',
                PHP_OS_FAMILY,
                $machine,
                self::SOURCE_HINT,
            ));
        }

        return [$os, $arch, $os === 'windows' ? '.exe' : ''];
    }

    private static function asset(): string
    {
        [$os, $arch, $ext] = self::target();

        return self::NAME . "_{$os}_{$arch}{$ext}";
    }

    private static function version(): string
    {
        $file = dirname(__DIR__) . '/package.json';
        $pkg = json_decode((string) @file_get_contents($file), true);
        if (!is_array($pkg) || !is_string($pkg['version'] ?? null)) {
            throw new \RuntimeException("Cannot read the version from {$file}");
        }

        return $pkg['version'];
    }

    private static function fetch(string $cached): void
    {
        self::inTmpDir($cached, static function (string $tmp) use ($cached): void {
            self::commit(self::fetchRelease('v' . self::version(), self::asset(), $tmp), $cached);
        });
    }

    private static function cacheDir(): string
    {
        return self::cacheRoot() . '/' . self::NAME . '/' . self::version();
    }

    private static function cacheRoot(): string
    {
        $xdg = getenv('XDG_CACHE_HOME');
        if (is_string($xdg) && $xdg !== '') {
            return $xdg;
        }
        [$var, $suffix] = match (PHP_OS_FAMILY) {
            'Windows' => ['LOCALAPPDATA', ''],
            'Darwin' => ['HOME', '/Library/Caches'],
            default => ['HOME', '/.cache'],
        };
        $base = getenv($var);
        if (!is_string($base) || $base === '') {
            throw new \RuntimeException("Cannot locate a cache directory: neither XDG_CACHE_HOME nor {$var} is set");
        }

        return $base . $suffix;
    }

    private static function present(string $file): bool
    {
        return is_file($file) && filesize($file) > 0;
    }

    private static function place(string $cached, string $dest): void
    {
        $tmp = self::tmpBeside($dest);
        try {
            if (!@link($cached, $tmp) && !@copy($cached, $tmp)) {
                throw new \RuntimeException("Cannot link or copy {$cached} to {$tmp}: " . self::lastError());
            }
            self::commit($tmp, $dest);
        } finally {
            @unlink($tmp);
        }
    }

    private static function commit(string $tmp, string $dest): void
    {
        if (!@chmod($tmp, 0755) || !@rename($tmp, $dest)) {
            throw new \RuntimeException("Cannot move {$tmp} into place at {$dest}: " . self::lastError());
        }
    }

    private static function tmpBeside(string $file): string
    {
        $dir = dirname($file);
        if (!is_dir($dir) && !@mkdir($dir, 0755, true) && !is_dir($dir)) {
            throw new \RuntimeException("Cannot create {$dir}: " . self::lastError());
        }

        return "{$file}." . bin2hex(random_bytes(6)) . '.tmp';
    }

    private static function inTmpDir(string $beside, callable $fn): void
    {
        $tmp = self::tmpBeside($beside);
        if (!@mkdir($tmp, 0755)) {
            throw new \RuntimeException("Cannot create {$tmp}: " . self::lastError());
        }
        try {
            $fn($tmp);
        } finally {
            array_map('unlink', glob("{$tmp}/*") ?: []);
            @rmdir($tmp);
        }
    }

    private static function fetchRelease(string $tag, string $asset, string $dir): string
    {
        $base = sprintf('https://github.com/%s/releases/download/%s', self::REPO, $tag);
        self::download("{$base}/checksums.txt", "{$dir}/checksums.txt");
        $sums = (string) file_get_contents("{$dir}/checksums.txt");
        if (!preg_match('/^([0-9a-f]{64})\s+\*?' . preg_quote($asset, '/') . '$/m', $sums, $m)) {
            throw new \RuntimeException("{$asset} is not listed in {$tag} checksums.txt");
        }
        $file = "{$dir}/{$asset}";
        self::download("{$base}/{$asset}", $file);
        if (!hash_equals($m[1], hash_file('sha256', $file))) {
            throw new \RuntimeException("Checksum mismatch for {$asset} from {$tag}");
        }

        return $file;
    }

    private static function download(string $url, string $file): void
    {
        $out = @fopen($file, 'wb');
        if ($out === false) {
            throw new \RuntimeException("Cannot write {$file}: " . self::lastError());
        }
        try {
            if (function_exists('curl_init')) {
                self::curl($url, $out);
            } else {
                self::stream($url, $out);
            }
        } finally {
            fclose($out);
        }
        clearstatcache(true, $file);
        if (filesize($file) === 0) {
            throw new \RuntimeException("Failed to download {$url}: empty response");
        }
    }

    /** @param resource $out */
    private static function curl(string $url, $out): void
    {
        $ch = curl_init($url);
        curl_setopt_array($ch, [
            CURLOPT_FILE => $out,
            CURLOPT_FOLLOWLOCATION => true,
            CURLOPT_FAILONERROR => true,
            CURLOPT_USERAGENT => self::NAME . '-composer',
            CURLOPT_CONNECTTIMEOUT => 30,
            CURLOPT_LOW_SPEED_LIMIT => 1024,
            CURLOPT_LOW_SPEED_TIME => 30,
        ]);
        if (curl_exec($ch) === false) {
            throw new \RuntimeException("Failed to download {$url}: " . curl_error($ch));
        }
    }

    /** @param resource $out */
    private static function stream(string $url, $out): void
    {
        $ctx = stream_context_create(['http' => [
            'follow_location' => 1,
            'user_agent' => self::NAME . '-composer',
            'timeout' => 30,
        ]]);
        $in = @fopen($url, 'rb', false, $ctx);
        if ($in === false) {
            throw new \RuntimeException("Failed to download {$url}: " . self::lastError());
        }
        try {
            if (stream_copy_to_stream($in, $out) === false) {
                throw new \RuntimeException("Failed to download {$url}: " . self::lastError());
            }
        } finally {
            fclose($in);
        }
    }

    private static function lastError(): string
    {
        return error_get_last()['message'] ?? 'unknown error';
    }
}

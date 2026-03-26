require "download_strategy"

# Strategy for downloading assets from private GitHub repository releases.
# Requires HOMEBREW_GITHUB_API_TOKEN to be set with repo read access.
class GitHubPrivateRepositoryReleaseDownloadStrategy < CurlDownloadStrategy
  def initialize(url, name, version, **meta)
    super
    parse_url_pattern
    set_github_token
  end

  def parse_url_pattern
    url_pattern = %r{https://github.com/([^/]+)/([^/]+)/releases/download/([^/]+)/(.+)}
    unless @url =~ url_pattern
      raise CurlDownloadStrategyError, "Invalid url pattern for GitHub Release."
    end

    _, @owner, @repo, @tag, @filename = *@url.match(url_pattern)
  end

  def download_url
    "https://api.github.com/repos/#{@owner}/#{@repo}/releases/assets/#{asset_id}"
  end

  private

  def _fetch(url:, resolved_url:, timeout:)
    # Download via GitHub API with Accept header for binary content
    curl_download(
      download_url,
      "--header", "Authorization: token #{@github_token}",
      "--header", "Accept: application/octet-stream",
      to: temporary_path,
      timeout: timeout,
    )
  end

  def asset_id
    @asset_id ||= resolve_asset_id
  end

  def resolve_asset_id
    release_url = "https://api.github.com/repos/#{@owner}/#{@repo}/releases/tags/#{@tag}"
    response = JSON.parse(
      Utils::Curl.curl_output(
        release_url,
        "--header", "Authorization: token #{@github_token}",
        "--header", "Accept: application/vnd.github.v3+json",
      ).stdout,
    )

    assets = response["assets"]
    asset = assets.find { |a| a["name"] == @filename }
    raise CurlDownloadStrategyError, "Asset not found: #{@filename}" unless asset

    asset["id"]
  end

  def set_github_token
    @github_token = ENV["HOMEBREW_GITHUB_API_TOKEN"]
    unless @github_token
      raise CurlDownloadStrategyError, "HOMEBREW_GITHUB_API_TOKEN is required for private repo downloads"
    end
  end
end

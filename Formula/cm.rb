class Cm < Formula
  desc "SSH, VNC, and rsync profile manager"
  homepage "https://github.com/zhangwenqiang-dev/homebrew-tools"
  url "https://github.com/zhangwenqiang-dev/homebrew-tools.git",
      tag: "v0.1.135"
  license "MIT"

  depends_on "go" => :build

  def install
    system "go", "build", "-ldflags", "-X main.version=#{version}", "-o", bin/"cm", "./cmd/cm"
    pkgshare.install "web"
    generate_completions_from_executable(bin/"cm", "completion")
  end

  test do
    assert_match "Usage:", shell_output("#{bin}/cm --help")
    assert_match version.to_s, shell_output("#{bin}/cm version")
    assert_path_exists pkgshare/"web/index.html"
  end
end

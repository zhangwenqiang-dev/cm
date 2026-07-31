(function (window) {
  "use strict";

  const ACTIVE_JOB_STATUSES = new Set(["starting", "running", "deferred"]);
  const ACTIVE_LIFECYCLE_STATES = new Set(["pending", "waiting"]);
  const ACTIVE_AUTO_RELEASE_STATES = new Set(["running", "retrying", "notifying"]);

  function effectiveState(input) {
    const model = input || {};
    const status = Object.prototype.hasOwnProperty.call(model, "status") ? model.status : model;
    const profileName = model.profileName || model.profile || "";
    const jobs = Array.isArray(model.jobs) ? model.jobs : [];
    const reminder = model.reminder || model.autoRelease || model.auto_release || null;

    const releasingJob = jobs.some((job) => {
      if (!job || job.type !== "aws-destroy") return false;
      if (profileName && job.profile !== profileName) return false;
      return ACTIVE_JOB_STATUSES.has(job.status) ||
        ACTIVE_LIFECYCLE_STATES.has(job.lifecycle_state || job.lifecycleState);
    });
    const autoReleaseState = reminder &&
      (reminder.auto_release_state || reminder.autoReleaseState || reminder.state);

    if (releasingJob || ACTIVE_AUTO_RELEASE_STATES.has(autoReleaseState)) {
      return "releasing";
    }
    if (!status || status.error) return "unknown";
    if (status.ready === true) return "ready";

    switch (status.decision) {
      case "create":
        return "stopped";
      case "wait-ready":
      case "launch-on-host":
        return "creating";
      case "ready":
        return "ready";
      case "blocked":
      case "error":
        return "blocked";
      default:
        return "unknown";
    }
  }

  function action(visible, enabled, reason) {
    return {
      visible: visible,
      enabled: enabled,
      reason: enabled ? "" : (reason || ""),
    };
  }

  function buildActionModel(input) {
    const options = input || {};
    const state = options.state || options.effectiveState || "unknown";
    const isMobile = options.isMobile === true || options.mobile === true;
    const localAgentOnline = options.localAgentOnline === true ||
      (options.localAgent && options.localAgent.online === true);
    const releasing = state === "releasing";
    const ready = state === "ready";
    const known = state !== "unknown";
    const releaseReason = releasing ? "Mac 正在释放" : "";
    const notReadyReason = "Mac 尚未就绪";

    let localReason = "";
    if (isMobile) {
      localReason = "移动端不可用";
    } else if (!ready) {
      localReason = releasing ? releaseReason : notReadyReason;
    } else if (!localAgentOnline) {
      localReason = "本机代理未连接";
    }
    const localEnabled = !isMobile && ready && localAgentOnline && !releasing;

    const model = {
      primary: !isMobile && localEnabled ? "connect" : "refresh",
      refresh: action(true, true, ""),
      open: action(true, known && !ready && !releasing, releasing ? releaseReason : (ready ? "Mac 已就绪" : "状态未知")),
      release: action(true, ready && !releasing, releasing ? releaseReason : notReadyReason),
      connect: action(!isMobile, localEnabled, localReason),
      vnc: action(!isMobile, localEnabled, localReason),
      transfer: action(!isMobile, localEnabled, localReason),
      extend: action(true, ready && !releasing, releasing ? releaseReason : notReadyReason),
      cleanup: action(true, known && !ready && !releasing, releasing ? releaseReason : (ready ? "Mac 已就绪" : "状态未知")),
      events: action(true, true, ""),
      details: action(true, true, ""),
    };

    return model;
  }

  window.ConnectMacWorkbench = {
    effectiveState: effectiveState,
    buildActionModel: buildActionModel,
  };
})(window);

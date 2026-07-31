(function (window) {
  "use strict";

  const ACTIVE_JOB_STATUSES = new Set(["starting", "running", "deferred"]);
  const TERMINAL_JOB_STATUSES = new Set(["failed", "interrupted"]);
  const ACTIVE_LIFECYCLE_STATES = new Set(["pending", "waiting"]);
  const ACTIVE_AUTO_RELEASE_STATES = new Set(["running", "retrying", "notifying"]);
  const EFFECTIVE_STATES = new Set([
    "unknown",
    "stopped",
    "creating",
    "ready",
    "releasing",
    "blocked",
  ]);

  function activeLifecycleJob(job, type, profileName) {
    if (!job || job.type !== type) return false;
    if (job.profile !== profileName) return false;
    if (TERMINAL_JOB_STATUSES.has(job.status)) return false;

    const lifecycleState = String(job.lifecycle_state || job.lifecycleState || "").trim();
    if (lifecycleState) return ACTIVE_LIFECYCLE_STATES.has(lifecycleState);
    return ACTIVE_JOB_STATUSES.has(job.status);
  }

  function effectiveState(input) {
    const model = input || {};
    const status = Object.prototype.hasOwnProperty.call(model, "status") ? model.status : model;
    const profileName = model.profileName || model.profile || status?.profile || "";
    const jobs = Array.isArray(model.jobs) ? model.jobs : [];
    const reminder = model.reminder || model.autoRelease || model.auto_release || null;

    const releasingJob = !!profileName &&
      jobs.some((job) => activeLifecycleJob(job, "aws-destroy", profileName));
    const creatingJob = !!profileName &&
      jobs.some((job) => activeLifecycleJob(job, "aws-open", profileName));
    const autoReleaseState = reminder &&
      (reminder.auto_release_state || reminder.autoReleaseState || reminder.state);

    if (releasingJob || ACTIVE_AUTO_RELEASE_STATES.has(autoReleaseState)) {
      return "releasing";
    }
    if (creatingJob) return "creating";
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
    const isVisible = visible === true;
    const isEnabled = isVisible && enabled === true;
    return {
      visible: isVisible,
      enabled: isEnabled,
      reason: isEnabled ? "" : (reason || ""),
    };
  }

  function buildActionModel(input) {
    const options = input || {};
    const requestedState = options.effectiveState || options.state || "unknown";
    const state = EFFECTIVE_STATES.has(requestedState) ? requestedState : "unknown";
    const mobile = options.mobile === true || options.isMobile === true;
    const localAgentOnline = options.localAgentOnline === true ||
      options.localAgent?.online === true;
    const busy = options.busy === true;
    const canOperate = options.canOperate === true;
    const canAdmin = options.canAdmin === true;
    const releasing = state === "releasing";
    const ready = state === "ready";
    const operateAllowed = canOperate && !busy;
    const releaseReason = "Mac 正在释放";
    const notReadyReason = "Mac 尚未就绪";
    const operateReason = busy ? "操作进行中" : "无操作权限";

    let localReason = "";
    if (mobile) {
      localReason = "移动端不可用";
    } else if (!operateAllowed) {
      localReason = operateReason;
    } else if (!ready) {
      localReason = notReadyReason;
    } else if (!localAgentOnline) {
      localReason = "本机代理未连接";
    }
    const localEnabled = !mobile && ready && localAgentOnline && operateAllowed;
    const primary = state === "stopped"
      ? "open"
      : (state === "ready" && !mobile
        ? "connect"
        : ((state === "creating" || state === "releasing") ? "details" : "refresh"));

    const actions = {
      refresh: action(true, true, ""),
      open: action(true, state === "stopped" && operateAllowed, !operateAllowed ? operateReason : (releasing ? releaseReason : "Mac 当前不可打开")),
      release: action(true, ready && operateAllowed, !operateAllowed ? operateReason : (releasing ? releaseReason : notReadyReason)),
      connect: action(!mobile, localEnabled, localReason),
      vnc: action(!mobile, localEnabled, localReason),
      transfer: action(!mobile, localEnabled, localReason),
      extend: action(true, ready && operateAllowed, !operateAllowed ? operateReason : (releasing ? releaseReason : notReadyReason)),
      cleanup: action(
        canAdmin,
        canAdmin && state === "stopped" && !busy,
        !canAdmin ? "需要管理员权限" : (busy ? "操作进行中" : (releasing ? releaseReason : (ready ? "Mac 已就绪" : "状态未知"))),
      ),
      events: action(true, true, ""),
      details: action(true, true, ""),
    };

    return {
      state: state,
      primary: primary,
      actions: actions,
    };
  }

  window.ConnectMacWorkbench = {
    effectiveState: effectiveState,
    buildActionModel: buildActionModel,
  };
})(window);

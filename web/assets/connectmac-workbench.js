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
  const stateCopyMap = {
    stopped: {
      badge: "已停止",
      heading: "这台 Mac 尚未运行",
      detail: "打开前会先展示 AWS 预览并要求确认。",
    },
    creating: {
      badge: "正在打开",
      heading: "正在打开这台 Mac",
      detail: "系统正在创建资源并等待 AWS 状态检查。",
    },
    ready: {
      badge: "已就绪",
      heading: "这台 Mac 已可使用",
      detail: "可以连接终端、打开 VNC 或传输文件。",
    },
    releasing: {
      badge: "正在释放",
      heading: "正在释放这台 Mac",
      detail: "系统正在终止实例并等待 Dedicated Host 可释放。",
    },
    blocked: {
      badge: "受阻",
      heading: "当前流程已停止",
      detail: "请查看阻塞原因；系统不会自动创建其他 Host 或终止 EC2。",
    },
    unknown: {
      badge: "状态未知",
      heading: "暂时无法确认 Mac 状态",
      detail: "请先刷新状态或诊断配置。",
    },
  };

  function stateCopy(state) {
    return stateCopyMap[state] || stateCopyMap.unknown;
  }

  function shouldApplyProfileRefresh(input) {
    const options = input || {};
    return options.startedGeneration === options.currentGeneration &&
      options.authenticated === true &&
      options.visible === true &&
      options.aborted === false;
  }

  function activeLifecycleJob(job, type, profileName) {
    if (!job || job.type !== type) return false;
    if (job.profile !== profileName) return false;
    if (TERMINAL_JOB_STATUSES.has(job.status)) return false;

    const lifecycleState = String(job.lifecycle_state || job.lifecycleState || "").trim();
    if (lifecycleState) return ACTIVE_LIFECYCLE_STATES.has(lifecycleState);
    return ACTIVE_JOB_STATUSES.has(job.status);
  }

  function activeLifecycleTask(jobs, profileName) {
    if (!profileName || !Array.isArray(jobs)) return null;
    const active = jobs.filter((job) =>
      activeLifecycleJob(job, "aws-destroy", profileName) ||
      activeLifecycleJob(job, "aws-open", profileName)
    );
    active.sort((left, right) => {
      if (left.type !== right.type) return left.type === "aws-destroy" ? -1 : 1;
      return Date.parse(right.started_at || "") - Date.parse(left.started_at || "");
    });
    const job = active[0];
    if (!job) return null;

    const lifecycleState = String(job.lifecycle_state || job.lifecycleState || "").trim();
    const destroying = job.type === "aws-destroy";
    let label = destroying ? "正在释放 AWS Mac" : "正在打开 AWS Mac";
    if (lifecycleState === "waiting") {
      label = destroying ? "等待 Dedicated Host 可释放" : "等待 Mac 状态检查";
    } else if (job.status === "starting") {
      label = destroying ? "正在启动释放任务" : "正在启动打开任务";
    } else if (job.status === "deferred") {
      label = destroying ? "释放任务等待后续检查" : "打开任务等待后续检查";
    }

    return {
      id: String(job.id || ""),
      type: job.type,
      label: label,
      request_id: String(job.request_id || job.requestID || ""),
      actor: String(job.actor_name || job.actor_email || job.actor || ""),
      started_at: String(job.started_at || job.startedAt || ""),
      terminal: false,
    };
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
    const hasProfile = options.hasProfile === true;
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
    let primary = "refresh";
    if (hasProfile) {
      primary = state === "stopped"
        ? "open"
        : (state === "ready" && !mobile
        ? "connect"
        : ((state === "creating" || state === "releasing") ? "details" : "refresh"));
    }

    const actions = {
      refresh: action(true, !busy, busy ? operateReason : ""),
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
      events: action(true, !busy, busy ? operateReason : ""),
      details: action(true, true, ""),
    };

    if (!hasProfile) {
      for (const name of Object.keys(actions)) {
        actions[name] = action(actions[name].visible, false, "请先选择 Profile");
      }
    }

    return {
      state: state,
      primary: primary,
      actions: actions,
    };
  }

  window.ConnectMacWorkbench = {
    activeLifecycleTask: activeLifecycleTask,
    effectiveState: effectiveState,
    buildActionModel: buildActionModel,
    stateCopyMap: stateCopyMap,
    stateCopy: stateCopy,
    shouldApplyProfileRefresh: shouldApplyProfileRefresh,
  };
})(window);

async function refreshPage(kind) {
  switch (kind) {
    case "agents":
      state = await api("agents/rescan", {});
      renderAgents();
      await Promise.all([loadInstalls(), loadCLIs()]);
      return t("Agents looked for again");
    case "usage":
      await magpie.call("usage", "reload");
      return t("Usage refreshed");
    case "allowances":
      await loadQuotas("all");
      return t("Allowances refreshed");
    case "requests":
      await loadPanelUse();
      return t("Usage refreshed");
  }
  state = await api("sync", {});
  const failed = state.modelRefreshFailed;
  renderAgents();
  if (providers) await loadProviders();
  if (failed?.length) {
    status(t("Could not refresh model lists for {providers}", { providers: failed.join(", ") }), "err");
    return;
  }
  return t("Model lists refreshed");
}

export async function runHeaderRefresh(button, kind) {
  try {
    const message = await refreshPage(kind);
    if (message) status(message, "ok");
  } catch (error) {
    status(kind === "models" ? t("Sync failed: {e}", { e: error.message }) : error.message, "err");
  } finally {
    button.blur();
    const spins = button.querySelector("svg").getAnimations({ subtree: true });
    const stop = () => button.classList.remove("spin");
    if (spins.length && spins.every((animation) => animation.effect?.updateTiming)) {
      for (const animation of spins) animation.effect.updateTiming({ iterations: (animation.effect.getComputedTiming().currentIteration || 0) + 1 });
      Promise.all(spins.map((animation) => animation.finished)).then(stop, stop);
    } else stop();
  }
}

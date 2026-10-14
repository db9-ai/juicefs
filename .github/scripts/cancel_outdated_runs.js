module.exports = async ({github, context, core}) => {
  const repo = context.repo;
  const pull = context.payload.pull_request;
  const currentHead = async () => (await github.rest.pulls.get({
    ...repo, pull_number: pull.number,
  })).data.head.sha;
  if (await currentHead() !== pull.head.sha) {
    core.info('A newer pull request revision superseded this cancellation run.');
    return;
  }
  for (const status of ['queued', 'in_progress', 'waiting', 'pending', 'requested']) {
    const runs = await github.paginate(github.rest.actions.listWorkflowRunsForRepo, {
      ...repo, event: 'pull_request', branch: pull.head.ref, status, per_page: 100,
    });
    for (const run of runs) {
      if (run.id === context.runId || run.head_sha === pull.head.sha ||
          !run.pull_requests.some(pr => pr.number === pull.number)) {
        continue;
      }
      if (await currentHead() !== pull.head.sha) {
        core.info('A newer pull request revision arrived; stopping cancellation.');
        return;
      }
      try {
        await github.rest.actions.cancelWorkflowRun({...repo, run_id: run.id});
        core.info(`Cancelled outdated pull request run ${run.id}.`);
      } catch (error) {
        if (error.status !== 409) throw error;
        const latest = await github.rest.actions.getWorkflowRun({...repo, run_id: run.id});
        if (latest.data.status !== 'completed') throw error;
        core.info(`Run ${run.id} completed before cancellation.`);
      }
    }
  }
};

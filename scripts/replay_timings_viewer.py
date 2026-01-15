# /// script
# requires-python = ">=3.12"
# dependencies = [
#     "altair==5.5.0",
#     "ipython==9.3.0",
#     "marimo",
# ]
# ///

import marimo

__generated_with = "0.13.15"
app = marimo.App(width="medium")


@app.cell
def _():
    import json

    latency_records = []

    # Path to replay_timings.jsonl - update to point to your run's log directory
    # Example: /mnt/mithril-logs/20260115-120000Z_abc123_def456/replay_timings.jsonl
    import sys
    if len(sys.argv) > 1:
        timings_path = sys.argv[1]
    else:
        timings_path = "/mnt/mithril-logs/latest/replay_timings.jsonl"

    with open(timings_path) as f:
        for l in f:
            record = json.loads(l)
            record["SlotCount"] = len(latency_records)
            # Flatten the structure.
            flat_record = {}
            for k, v in record.items():
                if type(v) == dict and "SumNanoseconds" in v:
                    flat_record[k] = v["SumNanoseconds"] / 1_000_000
                else:
                    flat_record[k] = v
            latency_records.append(flat_record)

    len(latency_records)
    latency_records[0]
    return (latency_records,)


@app.cell
def _():
    import marimo as mo
    import altair as alt
    return alt, mo


@app.cell(hide_code=True)
def _(alt, latency_records, mo):
    block_chart = (
        alt.Chart(alt.InlineData(values=latency_records))
        .transform_fold(
            [
                "PreprocessBlock",
                "LoadBlockAccounts",
                "TxLoop",
                "Reward",
                "Rent",
                "RunIncinerator",
                "BlockUpdateAccounts",
                "AccountsDeltaHash",
                "BankHash",
            ],
            as_=["Phase", "Latency"],
        )
        .mark_bar()
        .encode(x="SlotCount:O", y="Latency:Q", color="Phase:N")
    )
    mo.ui.altair_chart(block_chart)
    return


@app.cell(hide_code=True)
def _(alt, latency_records, mo):
    tx_chart = (
        alt.Chart(alt.InlineData(values=latency_records))
        .transform_fold(
            [
                "InstructionsAndAccountMetasFromTx",
                "ComputeBudgetExecutionInstructions",
                "AccountsFromTx",
                "PreBalanceDivergenceCheck",
                "CalcAndDeductFees",
                "ReadRentSysvar",
                "PreTxRentStates",
                "IxLoop",
                "PostTxRentStates",
                "PostBalanceDivergenceCheck",
                "TxUpdateAccounts",
            ],
            as_=["Phase", "Latency"],
        )
        .mark_bar()
        .encode(x="SlotCount:O", y="Latency:Q", color="Phase:N")
    )
    mo.ui.altair_chart(tx_chart)
    return


@app.cell(hide_code=True)
def _(alt, latency_records, mo):
    ix_chart = (
        alt.Chart(alt.InlineData(values=latency_records))
        .transform_fold(
            [
                "GetNextIxCtx",
                "NextIxCtxConfigure",
                "IxPush",
                "IxPop",
                "ExecIxResolveNativeProgram",
                "ExecIxNativeProgramSystem",
                "ExecIxNativeProgramStake",
                "ExecIxNativeProgramVote",
                "ExecIxNativeProgramComputeBudget",
                "ExecIxNativeProgramBpfLoader2",
                "ExecIxNativeProgramBpfLoaderDeprecated",
                "ExecIxNativeProgramBpfLoaderUpgradeable",
                "ExecIxNativeProgramZkElgamalProof",
                "ExecIxNativeProgramEd25519Precompile",
                "ExecIxNativeProgramSecp256kPrecompile",
                "FixupInstructionsSysvarAccount",
                "InstructionAccountsFromAccountMetas",
            ],
            as_=["Phase", "Latency"],
        )
        .mark_bar()
        .encode(x="SlotCount:O", y="Latency:Q", color="Phase:N")
    )
    mo.ui.altair_chart(ix_chart)
    return


@app.cell(hide_code=True)
def _(alt, latency_records, mo):
    sbpf_chart = (
        alt.Chart(alt.InlineData(values=latency_records))
        .transform_fold(
            [
                "SbpfInterpreterNew",
                "SbpfInterpreterRun",
                "AddProgramToCache",
                "GetProgramAccount",
                "GetProgramDataCached",
                "GetProgramDataUncachedAccountsDb",
                "GetProgramDataUncachedAccounts",
                "GetProgramDataUncachedMarshal",
            ],
            as_=["Phase", "Latency"],
        )
        .mark_bar()
        .encode(x="SlotCount:O", y="Latency:Q", color="Phase:N")
    )
    mo.ui.altair_chart(sbpf_chart)
    return


if __name__ == "__main__":
    app.run()

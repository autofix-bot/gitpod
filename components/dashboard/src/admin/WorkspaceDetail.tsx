import { WorkspaceAndInstance } from "@gitpod/gitpod-protocol";
import moment from "moment";
import { useState } from "react";
import { getGitpodService } from "../service/service";
import { getProject, WorkspaceStatusIndicator } from "../workspaces/WorkspaceEntry";
import { Property } from "./UserDetail";

export default function WorkspaceDetail(props: { workspace: WorkspaceAndInstance }) {
    const [workspace, setWorkspace] = useState(props.workspace);
    const [activity, setActivity] = useState(false);

    const stopWorkspace = async () => {
        try {
            setActivity(true);
            await getGitpodService().server.adminForceStopWorkspace(workspace.workspaceId);
            // let's reload in a sec
            setTimeout(reload, 2000);
        } finally {
            setActivity(false);
        }
    }

    const reload = async () => {
        try {
            setActivity(true);
            const ws = await getGitpodService().server.adminGetWorkspace(workspace.workspaceId);
            setWorkspace(ws);
        } finally {
            setActivity(false);
        }
    }

    return <>
        <div className="flex">
            <div className="flex-1">
                <div className="flex"><h3>{workspace.workspaceId}</h3><span className="my-auto ml-3"><WorkspaceStatusIndicator instance={WorkspaceAndInstance.toInstance(workspace)} /></span></div>
                <p>{getProject(WorkspaceAndInstance.toWorkspace(workspace))}</p>
            </div>
            <button className="secondary ml-3" disabled={activity} onClick={reload}>Reload Data</button>
            <button className="danger ml-3" disabled={activity || workspace.phase === 'stopped'} onClick={stopWorkspace}>Stop Workspaces</button>
        </div>
        <div className="flex mt-6">
            <div className="flex flex-col w-full">
                <div className="flex w-full mt-6">
                    <Property name="Created" value={moment(workspace.workspaceCreationTime).format('MMM D, YYYY')} />
                    <Property name="Last Start" value={moment(workspace.instanceCreationTime).format('MMM D, YYYY')} />
                    <Property name="Context" value={workspace.contextURL} />
                </div>
                <div className="flex w-full mt-6">

                </div>
            </div>
        </div>
    </>;
}
/**
 * Copyright (c) 2021 Gitpod GmbH. All rights reserved.
 * Licensed under the GNU Affero General Public License (AGPL).
 * See License-AGPL.txt in the project root for license information.
 */

import { AdminGetListResult, User, WorkspaceAndInstance } from "@gitpod/gitpod-protocol";
import moment from "moment";
import { useContext, useEffect, useState } from "react";
import { useLocation } from "react-router";
import { Link } from "react-router-dom";
import { PageWithSubMenu } from "../components/PageWithSubMenu";
import { getGitpodService } from "../service/service";
import { UserContext } from "../user-context";
import { getProject, WorkspaceStatusIndicator } from "../workspaces/WorkspaceEntry";
import { adminMenu } from "./admin-menu";
import WorkspaceDetail from "./WorkspaceDetail";

interface Props {
    user?: User
}

export default function WorkspaceSearchPage() {
    return <PageWithSubMenu subMenu={adminMenu} title="Workspaces" subtitle="Search and manage all workspaces.">
        <WorkspaceSearch />
    </PageWithSubMenu>;
}

export function WorkspaceSearch(props: Props) {
    const location = useLocation();
    const { user } = useContext(UserContext);
    const [searchResult, setSearchResult] = useState<AdminGetListResult<WorkspaceAndInstance>>({ rows: [], total: 0 });
    const [searchTerm, setSearchTerm] = useState('');
    const [searching, setSearching] = useState(false);
    const [currentWorkspace, setCurrentWorkspaceState] = useState<WorkspaceAndInstance|undefined>(undefined);

    useEffect(() => {
        const workspaceId = location.pathname.split('/')[3];
        if (workspaceId) {
            let user = searchResult.rows.find(ws => ws.workspaceId === workspaceId);
            if (user) {
                setCurrentWorkspaceState(user);
            } else {
                getGitpodService().server.adminGetWorkspace(workspaceId).then(
                    ws => setCurrentWorkspaceState(ws)
                ).catch(e => console.error(e));
            }
        } else {
            setCurrentWorkspaceState(undefined);
        }
    }, [location]);

    useEffect(() => {
        if (props.user) {
            search();
        }
    }, [props.user]);

    if (!user) { // } || !('admin' in (user?.rolesOrPermissions || []))) {
        return <></>;
    }

    if (currentWorkspace) {
        return <WorkspaceDetail workspace={currentWorkspace}/>;
    }

    const search = async () => {
        setSearching(true);
        try {
            const result = await getGitpodService().server.adminGetWorkspaces({
                searchTerm,
                limit: 100,
                orderBy: 'instanceCreationTime',
                offset: 0,
                orderDir: "desc",
                ownerId: props?.user?.id
            });
            setSearchResult(result);
        } finally {
            setSearching(false);
        }
    }
    const canSearch = searchTerm.length > 1;
    return <>
        <div className="pt-8 flex">
            <div className="flex justify-between w-full">
                <div className="flex">
                    <div className="py-4">
                        <svg className={searching ? 'animate-spin' : ''} width="16" height="16" fill="none" xmlns="http://www.w3.org/2000/svg">
                            <path fillRule="evenodd" clipRule="evenodd" d="M6 2a4 4 0 100 8 4 4 0 000-8zM0 6a6 6 0 1110.89 3.477l4.817 4.816a1 1 0 01-1.414 1.414l-4.816-4.816A6 6 0 010 6z" fill="#A8A29E" />
                        </svg>
                    </div>
                    <input className="border-0" type="text" placeholder="Search in context urls" onKeyDown={(ke) => ke.key === 'Enter' && canSearch && search() } onChange={(v) => { setSearchTerm(v.target.value) }} />
                </div>
                <button className="" disabled={!canSearch || searching} onClick={search}>Start Search</button>
            </div>
        </div>
        <div className="flex flex-col space-y-2">
            <div className="px-6 py-3 flex justify-between space-x-2 text-sm text-gray-400 border-t border-b border-gray-200">
                <div className="w-1/12"></div>
                <div className="w-6/12">Name</div>
                <div className="w-5/12">Created</div>
            </div>
        </div>
        {searchResult.rows.map(ws => <WorkspaceEntry ws={ws} />)}
    </>
}

function WorkspaceEntry(p: { ws: WorkspaceAndInstance }) {
    return <Link to={'/admin/workspaces/' + p.ws.workspaceId}>
        <div className="rounded-xl whitespace-nowrap flex space-x-2 py-6 px-6 w-full justify-between hover:bg-gray-100 focus:bg-gitpod-kumquat-light group">
            <div className="pr-3 self-center w-1/12">
                <WorkspaceStatusIndicator instance={WorkspaceAndInstance.toInstance(p.ws)}/>
            </div>
            <div className="flex flex-col w-4/12">
                <div className="font-medium text-gray-800 truncate hover:text-blue-600">{p.ws.workspaceId}</div>
                <div className="text-sm overflow-ellipsis truncate text-gray-400">{getProject(WorkspaceAndInstance.toWorkspace(p.ws))}</div>
            </div>
            <div className="flex w-4/12 self-center">
                <div className="flex flex-col">
                    <div className="text-gray-500 overflow-ellipsis truncate">{p.ws.description}</div>
                    <div className="text-sm text-gray-400 overflow-ellipsis truncate hover:text-blue-600">{p.ws.contextURL}</div>
                </div>
            </div>
            <div className="flex w-3/12 self-center">
                <div className="text-sm w-full text-gray-400 truncate">{moment(p.ws.instanceCreationTime || p.ws.workspaceCreationTime).fromNow()}</div>
            </div>
        </div>
    </Link>;
}
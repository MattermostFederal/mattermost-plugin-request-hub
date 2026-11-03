// Team creation, Team Admin, team member and team guest requests: every
// card goes to ~mattermost-team-requests in the approval team. Each type is
// submitted through REST, the inline slash command or the dialog and taken
// through approval or denial, and every refusal is checked to leave nothing
// behind (submit) or to leave the request pending (approval).
import type {Post} from '@mattermost/types/posts';

import {expect, test} from '../helpers/fixtures';
import {ADMIN, PLUGIN_ID, REQUEST_CHANNELS, cardRequestId, uid, type RequestHub, type TestUser} from '../helpers/requesthub';
import {
    ALREADY_HANDLED,
    DISABLED_MSG,
    OWN_NOMINATION,
    OWN_REQUEST,
    REQUESTER_INACTIVE,
    TYPE_OFF,
    actionIds,
    cardText,
    decide,
    expectNoRequest,
    expectResolved,
    expectStillPending,
    makeBot,
    pendingIds,
    recordsBy,
    submit,
    teamMember,
    withGuestAccounts,
} from '../helpers/teamrequests';
import {login, openChannel} from '../helpers/ui';

const SUBMITTED = {
    team: 'Your team creation request has been submitted for approval.',
    team_admin: 'Your Team Admin request has been submitted for approval.',
    team_member: 'Your team member request has been submitted for approval.',
    team_guest: 'Your guest invite request has been submitted for approval.',
};

const teamCard = (hub: RequestHub, needle: string) => hub.findCardIn(REQUEST_CHANNELS.team, needle);

// A new team made by the System Admin, with the given users as members.
async function makeTeam(hub: RequestHub, ...members: TestUser[]) {
    const admin = await hub.client(ADMIN);
    const name = uid('t');
    const team = await admin.createTeam({name, display_name: `Team ${name}`, type: 'O'} as Parameters<typeof admin.createTeam>[0]);
    for (const m of members) {
        await admin.addToTeam(team.id, m.id);
    }
    return team;
}

const archiveTeam = async (hub: RequestHub, teamId: string) => (await hub.client(ADMIN)).deleteTeam(teamId);
const deactivate = async (hub: RequestHub, user: TestUser) => (await hub.client(ADMIN)).updateUserActive(user.id, false);

async function submitDialog(hub: RequestHub, user: TestUser, route: string, callbackId: string, teamId: string, submission: Record<string, unknown>) {
    const client = await hub.client(user.username);
    const townSquare = await (await hub.client(ADMIN)).getChannelByName(hub.teamId, 'town-square');
    const res = await client.submitInteractiveDialog({
        url: `/plugins/${PLUGIN_ID}${route}`,
        callback_id: callbackId,
        state: teamId,
        user_id: user.id,
        channel_id: townSquare.id,
        // The form is opened from team "test"; the team it is about rides in
        // state, as the plugin sets it.
        team_id: hub.teamId,
        submission,
        cancelled: false,
    } as Parameters<typeof client.submitInteractiveDialog>[0]);
    return (res as {error?: string}).error;
}

async function cardFor(hub: RequestHub, requester: TestUser): Promise<[Post, string]> {
    const card = await teamCard(hub, `@${requester.username}`);
    return [card, cardRequestId(card)];
}

test.describe('team creation', () => {
    test('REST: System Admin approves, the team is created with the requester as Team Admin', async ({hub}) => {
        const requester = await hub.createUser();
        const slug = uid('team-');
        const display = `Team ${slug}`;
        const res = await submit(hub, requester, 'team', {display_name: display, name: slug, type: 'I', description: 'e2e team', request_team_admin: true});
        expect(res).toEqual({message: expect.stringContaining(SUBMITTED.team)});

        const card = await teamCard(hub, display);
        const id = cardRequestId(card);
        expect(card.message).toContain('a team creation request needs System Admin review');
        expect(actionIds(card)).toEqual(['approve', 'deny']);
        expect(cardText(card)).toContain('Team Admin Requested');
        await hub.findDM(requester.username, SUBMITTED.team);
        expect(await pendingIds(hub, 'team', requester.id)).toEqual([id]);
        expect(await hub.kv(`team_request_${id}`)).toMatchObject({name: slug, display_name: display, type: 'I', request_team_admin: true});

        // A regular user, the requester included, can't decide it.
        expect((await decide(hub, requester.username, 'team', card, id, true)).ephemeral_text).toBe('You aren\'t allowed to approve or deny team creation requests.');

        const admin = await hub.client(ADMIN);
        await admin.doPostAction(card.id, 'approve');
        await expectResolved(hub, 'team', requester, card, id, `Approved by @${ADMIN} for @${requester.username}. Team **${display}** created. Requester promoted to Team Admin.`);

        const team = await admin.getTeamByName(slug);
        expect(team).toMatchObject({display_name: display, type: 'I', description: 'e2e team'});
        expect((await teamMember(hub, team.id, requester.id))?.scheme_admin).toBe(true);
        await hub.findDM(requester.username, 'Team Creation Request Approved');
        await hub.findAudit(`@${ADMIN} approved team ${display} (${slug}) requested by @${requester.username}`);

        // A second click finds nothing left to decide.
        expect((await decide(hub, ADMIN, 'team', card, id, true)).ephemeral_text).toBe(ALREADY_HANDLED);
    });

    test('slash: an approved team without --team-admin makes the requester a plain member', async ({hub}) => {
        const requester = await hub.createUser();
        const word = uid('slash');
        const reply = await hub.slash(requester.username, `team Slash ${word} --description made inline`);
        expect(reply).toContain(SUBMITTED.team);

        const slug = `slash-${word}`;
        const card = await teamCard(hub, `Slash ${word}`);
        const id = cardRequestId(card);
        expect(await hub.kv(`team_request_${id}`)).toMatchObject({name: slug, type: 'O', request_team_admin: false, description: 'made inline'});

        await (await hub.client(ADMIN)).doPostAction(card.id, 'approve');
        await expectResolved(hub, 'team', requester, card, id, `Team **Slash ${word}** created.`);
        const team = await (await hub.client(ADMIN)).getTeamByName(slug);
        expect(team.type).toBe('O');
        const member = await teamMember(hub, team.id, requester.id);
        expect(member?.scheme_admin).toBe(false);
    });

    test('denied: no team is created and the requester is told', async ({hub}) => {
        const requester = await hub.createUser();
        const slug = uid('deny-');
        await hub.slash(requester.username, `team Deny ${slug} --name ${slug} --invite-only --team-admin`);
        const card = await teamCard(hub, slug);
        const id = cardRequestId(card);

        await (await hub.client(ADMIN)).doPostAction(card.id, 'deny');
        await expectResolved(hub, 'team', requester, card, id, `Denied by @${ADMIN} for @${requester.username}. Team **Deny ${slug}** (\`/${slug}\`).`);
        await expect((await hub.client(ADMIN)).getTeamByName(slug)).rejects.toMatchObject({status_code: 404});
        await hub.findDM(requester.username, 'Team Creation Request Denied');
        await hub.findAudit(`@${ADMIN} denied a team creation request for Deny ${slug}`);
    });

    test('refused at submit when team requests are off', async ({hub}) => {
        const requester = await hub.createUser();
        await hub.withPluginConfig({allowteamrequests: false}, async () => {
            expect(await submit(hub, requester, 'team', {display_name: uid('Off ')})).toEqual({error: DISABLED_MSG.team});
            expect(await hub.slash(requester.username, `team ${uid('Off ')}`)).toBe(DISABLED_MSG.team);
            expect(await submitDialog(hub, requester, '/api/v1/team_dialog', 'team_request', hub.teamId, {display_name: uid('Off ')})).toBe(DISABLED_MSG.team);
        });
        await expectNoRequest(hub, 'team', requester);
    });

    test('refused at submit: missing, invalid, reserved and over-long names and descriptions', async ({hub}) => {
        const requester = await hub.createUser();
        const cases: Array<[Record<string, unknown>, string]> = [
            [{display_name: '   '}, 'a team name is required'],
            [{display_name: 'Ok name', name: 'a'}, '"a" is not a valid team URL name; use 2-64 lowercase letters, numbers, and hyphens'],
            [{display_name: '!!!'}, '"" is not a valid team URL name'],
            [{display_name: 'Sign up', name: 'signup'}, '"signup" is reserved by Mattermost; choose a different team URL name'],
            [{display_name: 'x'.repeat(65)}, 'team name must be 64 characters or fewer'],
            [{display_name: 'Long description', description: 'd'.repeat(256)}, 'description must be 255 bytes or fewer'],
        ];
        for (const [body, error] of cases) {
            const res = await submit(hub, requester, 'team', body);
            expect(res.error, JSON.stringify(body)).toContain(error);
        }
        expect(await hub.slash(requester.username, 'team --name abc')).toContain('Could not read the request: a team name is required.');
        expect(await hub.slash(requester.username, 'team Name --colour red')).toContain('Could not read the request: unknown option');
        expect(await hub.slash(requester.username, 'team Name --invite-only yes')).toContain('`--invite-only` takes no value');
        await expectNoRequest(hub, 'team', requester);
    });

    test('refused at submit: a second pending request for the same URL name', async ({hub}) => {
        const requester = await hub.createUser();
        const slug = uid('dup-');
        expect((await submit(hub, requester, 'team', {display_name: slug})).message).toContain(SUBMITTED.team);
        const res = await submit(hub, requester, 'team', {display_name: `Other ${slug}`, name: slug});
        expect(res).toEqual({error: `you already have a pending request for a team named ${slug}`});
        expect(await recordsBy(hub, 'team', requester.id)).toHaveLength(1);
        expect(await pendingIds(hub, 'team', requester.id)).toHaveLength(1);
    });

    test('refused at submit when the URL name is already taken', async ({hub}) => {
        test.fail(true, 'BUG: submitTeamCreationRequest never checks GetTeamByName; a taken URL only fails when a System Admin approves');
        const requester = await hub.createUser();
        const taken = (await makeTeam(hub)).name;
        const res = await submit(hub, requester, 'team', {display_name: 'Taken', name: taken});
        expect(res.error).toBeTruthy();
        await expectNoRequest(hub, 'team', requester);
    });

    test.describe(() => {
        test.use({allowedPluginErrors: [/failed to create team on approval/]});

        test('refused at approval when the URL name was taken meanwhile; deny still closes it', async ({hub}) => {
            const requester = await hub.createUser();
            const slug = uid('clash-');
            await submit(hub, requester, 'team', {display_name: slug});
            const card = await teamCard(hub, slug);
            const id = cardRequestId(card);

            const admin = await hub.client(ADMIN);
            const squatter = await admin.createTeam({name: slug, display_name: 'Squatter', type: 'O'} as Parameters<typeof admin.createTeam>[0]);
            const reply = (await decide(hub, ADMIN, 'team', card, id, true)).ephemeral_text;
            expect(reply).toMatch(/^Could not create the team: .+\. The request is still pending\.$/);
            await expectStillPending(hub, 'team', requester, card, id);
            expect(await teamMember(hub, squatter.id, requester.id)).toBeUndefined();

            await admin.doPostAction(card.id, 'deny');
            await expectResolved(hub, 'team', requester, card, id, 'Denied by');
        });
    });

    test('refused at approval: requester deactivated, type turned off, non-approvers', async ({hub}) => {
        const requester = await hub.createUser();
        const slug = uid('recheck-');
        await submit(hub, requester, 'team', {display_name: slug});
        const card = await teamCard(hub, slug);
        const id = cardRequestId(card);
        const admin = await hub.client(ADMIN);

        // tadmin is a Team Admin of the approval team, which isn't enough
        // with AllowApprovalTeamAdminsForTeamRequests off, even when Team
        // Admins may approve channel requests.
        const ta = await hub.createUser({teamAdmin: true});
        await hub.withPluginConfig({allowteamadminapprovers: true}, async () => {
            expect((await decide(hub, ta.username, 'team', card, id, true)).ephemeral_text).toBe('You aren\'t allowed to approve or deny team creation requests.');
            expect((await decide(hub, ta.username, 'team', card, id, false)).ephemeral_text).toBe('You aren\'t allowed to approve or deny team creation requests.');
        });

        await hub.withPluginConfig({allowteamrequests: false}, async () => {
            expect((await decide(hub, ADMIN, 'team', card, id, true)).ephemeral_text).toBe(TYPE_OFF);
        });
        await expectStillPending(hub, 'team', requester, card, id);

        await deactivate(hub, requester);
        expect((await decide(hub, ADMIN, 'team', card, id, true)).ephemeral_text).toBe(REQUESTER_INACTIVE);
        await expectStillPending(hub, 'team', requester, card, id);
        await expect(admin.getTeamByName(slug)).rejects.toMatchObject({status_code: 404});

        await admin.doPostAction(card.id, 'deny');
        await expectResolved(hub, 'team', requester, card, id, 'Denied by');
    });

    test('approval team Team Admins decide team requests only with AllowApprovalTeamAdminsForTeamRequests on, never their own', async ({hub}) => {
        const ta = await hub.createUser({teamAdmin: true});
        const requester = await hub.createUser();
        await hub.withPluginConfig({allowapprovalteamadminsforteamrequests: true}, async () => {
            const slug = uid('byta-');
            await submit(hub, requester, 'team', {display_name: slug});
            const card = await teamCard(hub, slug);
            const id = cardRequestId(card);
            // The card mentions the approval team's Team Admins, who are
            // added to the channel so they can click.
            expect(card.message).toContain(`@${ta.username}`);
            expect(card.message).toContain('a team creation request needs your review');
            const taClient = await hub.client(ta.username);
            await taClient.doPostAction(card.id, 'approve');
            await expectResolved(hub, 'team', requester, card, id, `Approved by @${ta.username}`);
            expect(await teamMember(hub, (await (await hub.client(ADMIN)).getTeamByName(slug)).id, requester.id)).toBeDefined();

            // Their own request needs another approver.
            const own = uid('own-');
            await submit(hub, ta, 'team', {display_name: own});
            const ownCard = await teamCard(hub, own);
            const ownId = cardRequestId(ownCard);
            expect((await decide(hub, ta.username, 'team', ownCard, ownId, true)).ephemeral_text).toBe(OWN_REQUEST);
            await expectStillPending(hub, 'team', ta, ownCard, ownId);
            await taClient.doPostAction(ownCard.id, 'deny');
            await expectResolved(hub, 'team', ta, ownCard, ownId, `Denied by @${ta.username}`);
        });
    });
});

test.describe('Team Admin', () => {
    test('REST: self-nomination approved by a System Admin', async ({hub}) => {
        const requester = await hub.createUser();
        const res = await submit(hub, requester, 'team_admin', {team_id: hub.teamId});
        expect(res).toEqual({message: expect.stringContaining(SUBMITTED.team_admin)});

        const [card, id] = await cardFor(hub, requester);
        expect(card.message).toContain('a Team Admin request needs System Admin review');
        expect(cardText(card)).toContain('Proposed Team Admins');
        await hub.findDM(requester.username, SUBMITTED.team_admin);
        expect(await hub.kv(`team_admin_request_${id}`)).toMatchObject({team_id: hub.teamId, nominee_ids: [requester.id]});
        expect(await pendingIds(hub, 'team_admin', requester.id)).toEqual([id]);

        await (await hub.client(ADMIN)).doPostAction(card.id, 'approve');
        await expectResolved(hub, 'team_admin', requester, card, id, `Approved by @${ADMIN} for @${requester.username}. @${requester.username} promoted to Team Admin in **Test**.`);
        expect((await teamMember(hub, hub.teamId, requester.id))?.scheme_admin).toBe(true);
        await hub.findDM(requester.username, 'Team Admin Request Approved');
        await hub.findAudit(`@${ADMIN} promoted @${requester.username} to Team Admin in **Test** (requested by @${requester.username})`);
    });

    test('slash: nominating two members, approved', async ({hub}) => {
        const requester = await hub.createUser();
        const a = await hub.createUser();
        const b = await hub.createUser();
        expect(await hub.slash(requester.username, `team-admin @${a.username} ${b.username}`)).toContain(SUBMITTED.team_admin);
        const [card, id] = await cardFor(hub, requester);
        expect(await hub.kv(`team_admin_request_${id}`)).toMatchObject({nominee_ids: [a.id, b.id]});

        await (await hub.client(ADMIN)).doPostAction(card.id, 'approve');
        await expectResolved(hub, 'team_admin', requester, card, id, `@${a.username}, @${b.username} promoted to Team Admin`);
        expect((await teamMember(hub, hub.teamId, a.id))?.scheme_admin).toBe(true);
        expect((await teamMember(hub, hub.teamId, b.id))?.scheme_admin).toBe(true);
        expect((await teamMember(hub, hub.teamId, requester.id))?.scheme_admin).toBe(false);
    });

    test('dialog: nominee in another team, denied', async ({hub}) => {
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        const team = await makeTeam(hub, requester, nominee);
        expect(await submitDialog(hub, requester, '/api/v1/team_admin_dialog', 'team_admin_request', team.id, {members: nominee.id})).toBeUndefined();
        const [card, id] = await cardFor(hub, requester);

        await (await hub.client(ADMIN)).doPostAction(card.id, 'deny');
        await expectResolved(hub, 'team_admin', requester, card, id, `Denied by @${ADMIN} for @${requester.username}. Team Admin for @${nominee.username} in **${team.display_name}**.`);
        expect((await teamMember(hub, team.id, nominee.id))?.scheme_admin).toBe(false);
        await hub.findDM(requester.username, 'Team Admin Request Denied');
        await hub.findAudit(`@${ADMIN} denied a Team Admin request for @${nominee.username} in **${team.display_name}**`);
    });

    test('refused at submit when Team Admin requests are off', async ({hub}) => {
        const requester = await hub.createUser();
        await hub.withPluginConfig({allowteamadminrequests: false}, async () => {
            expect(await submit(hub, requester, 'team_admin', {team_id: hub.teamId})).toEqual({error: DISABLED_MSG.team_admin});
            expect(await hub.slash(requester.username, `team-admin @${requester.username}`)).toBe(DISABLED_MSG.team_admin);
            expect(await submitDialog(hub, requester, '/api/v1/team_admin_dialog', 'team_admin_request', hub.teamId, {members: requester.id})).toBe(DISABLED_MSG.team_admin);
        });
        await expectNoRequest(hub, 'team_admin', requester);
    });

    test('refused at submit: unknown, deactivated, bot and off-team nominees, bad teams, outsiders', async ({hub}) => {
        const requester = await hub.createUser();
        const admin = await hub.client(ADMIN);
        const gone = await hub.createUser();
        await deactivate(hub, gone);
        const outsider = await hub.createUser({inTeam: false});
        const bot = await makeBot(hub, uid('bot'));
        await admin.addToTeam(hub.teamId, bot.user_id);
        const otherTeam = await makeTeam(hub);
        const dialog = (teamId: string, members: string) => submitDialog(hub, requester, '/api/v1/team_admin_dialog', 'team_admin_request', teamId, {members});

        expect(await hub.slash(requester.username, `team-admin @${uid('nobody')}`)).toMatch(/^unknown user: nobody/);
        expect(await hub.slash(requester.username, 'team-admin')).toBe(''); // opens the form instead
        expect(await hub.slash(requester.username, `team-admin @${gone.username}`)).toBe(`@${gone.username} is deactivated and can't be made a Team Admin`);
        expect(await hub.slash(requester.username, `team-admin @${bot.username}`)).toBe(`@${bot.username} is a bot and can't be made a Team Admin`);
        expect(await hub.slash(requester.username, `team-admin @${outsider.username}`)).toBe(`@${outsider.username} isn't a member of this team`);
        expect(await hub.slash(requester.username, `team-admin @${requester.username} --now`)).toContain('Could not read the request: unknown option');
        expect(await dialog(hub.teamId, '')).toBe('pick at least one person to make a Team Admin');
        expect(await dialog(hub.teamId, 'not-an-id')).toBe('Team Admin nominees must be picked from the list');
        expect(await dialog(hub.teamId, 'a'.repeat(26))).toBe('one of the people you picked could not be found');
        expect(await dialog(hub.teamId, Array.from({length: 101}, (_, i) => `a${String(i).padStart(25, '0')}`).join(','))).toBe('too many Team Admin nominees: at most 100 per request');
        expect(await dialog('b'.repeat(26), requester.id)).toBe('could not find that team');
        expect(await submit(hub, requester, 'team_admin', {team_id: ''})).toEqual({error: 'team_id is required'});
        expect(await submit(hub, requester, 'team_admin', {team_id: otherTeam.id})).toEqual({error: 'you must be a member of the team to request Team Admin access'});
        await expectNoRequest(hub, 'team_admin', requester);
    });

    test('refused at submit: a second pending request for the same people', async ({hub}) => {
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        await hub.slash(requester.username, `team-admin @${nominee.username} @${requester.username}`);
        expect(await hub.slash(requester.username, `team-admin ${requester.username},${nominee.username}`)).toBe('you already have a pending Team Admin request for these people in this team');
        expect(await recordsBy(hub, 'team_admin', requester.id)).toHaveLength(1);
    });

    test('refused at submit when every nominee is already a Team Admin', async ({hub}) => {
        test.fail(true, 'BUG: checkNominees does not skip or refuse nominees who are already Team Admin; the request queues and approval is a no-op');
        const requester = await hub.createUser();
        const ta = await hub.createUser({teamAdmin: true});
        expect(await hub.slash(requester.username, `team-admin @${ta.username}`)).not.toContain(SUBMITTED.team_admin);
        await expectNoRequest(hub, 'team_admin', requester);
    });

    test('nominees and requesters who are approvers cannot approve their own promotion', async ({hub}) => {
        // Both are Team Admins of the approval team, so they may decide
        // Team Admin requests with the setting on, and members of team X.
        const requester = await hub.createUser({teamAdmin: true});
        const nominee = await hub.createUser({teamAdmin: true});
        const team = await makeTeam(hub, requester, nominee);
        await hub.withPluginConfig({allowapprovalteamadminsforteamrequests: true}, async () => {
            await submitDialog(hub, requester, '/api/v1/team_admin_dialog', 'team_admin_request', team.id, {members: nominee.id});
            const [card, id] = await cardFor(hub, requester);
            expect((await decide(hub, nominee.username, 'team_admin', card, id, true)).ephemeral_text).toBe(OWN_NOMINATION);
            expect((await decide(hub, requester.username, 'team_admin', card, id, true)).ephemeral_text).toBe(OWN_REQUEST);
            await expectStillPending(hub, 'team_admin', requester, card, id);
            expect((await teamMember(hub, team.id, nominee.id))?.scheme_admin).toBe(false);

            // Deny is not self-serving, so the nominee may close it.
            await (await hub.client(nominee.username)).doPostAction(card.id, 'deny');
            await expectResolved(hub, 'team_admin', requester, card, id, `Denied by @${nominee.username}`);
        });
    });

    test('Team Admins: the approval team\'s may decide only with the setting on, the target team\'s never', async ({hub}) => {
        const approvalTa = await hub.createUser({teamAdmin: true});
        const requester = await hub.createUser();
        const team = await makeTeam(hub, requester);
        const targetTa = await hub.createUser({inTeam: false});
        const admin = await hub.client(ADMIN);
        await admin.addToTeam(team.id, targetTa.id);
        await admin.updateTeamMemberSchemeRoles(team.id, targetTa.id, true, true);
        const refused = 'You aren\'t allowed to approve or deny Team Admin requests.';

        await submit(hub, requester, 'team_admin', {team_id: team.id});
        const [card, id] = await cardFor(hub, requester);
        await hub.withPluginConfig({allowteamadminapprovers: true}, async () => {
            expect((await decide(hub, approvalTa.username, 'team_admin', card, id, true)).ephemeral_text).toBe(refused);
        });
        await hub.withPluginConfig({allowapprovalteamadminsforteamrequests: true}, async () => {
            expect((await decide(hub, targetTa.username, 'team_admin', card, id, true)).ephemeral_text).toBe(refused);
            expect((await decide(hub, targetTa.username, 'team_admin', card, id, false)).ephemeral_text).toBe(refused);
            await expectStillPending(hub, 'team_admin', requester, card, id);
            expect((await teamMember(hub, team.id, requester.id))?.scheme_admin).toBe(false);

            // The card predates the setting, so the approval team's Team
            // Admin uses the route rather than a click on a channel they
            // were never added to.
            const res = await decide(hub, approvalTa.username, 'team_admin', card, id, true);
            expect(res.ephemeral_text ?? '').toBe('');
            expect(res.update?.message).toContain(`Approved by @${approvalTa.username}`);
        });
        expect((await teamMember(hub, team.id, requester.id))?.scheme_admin).toBe(true);
        await hub.findAudit(`@${approvalTa.username} promoted @${requester.username} to Team Admin in **${team.display_name}**`);
    });

    test('refused at approval: re-checks of requester, nominees and team leave the request pending', async ({hub}) => {
        const admin = await hub.client(ADMIN);
        const approve = async (card: Post, id: string) => (await decide(hub, ADMIN, 'team_admin', card, id, true)).ephemeral_text;

        // Requester left the team.
        const r1 = await hub.createUser();
        const team1 = await makeTeam(hub, r1);
        await submit(hub, r1, 'team_admin', {team_id: team1.id});
        const [c1, id1] = await cardFor(hub, r1);
        await admin.removeFromTeam(team1.id, r1.id);
        expect(await approve(c1, id1)).toBe('The requester is no longer a member of the team. Deny this request instead.');
        await expectStillPending(hub, 'team_admin', r1, c1, id1);
        expect(await teamMember(hub, team1.id, r1.id)).toBeUndefined();

        // Requester deactivated.
        const r2 = await hub.createUser();
        const team2 = await makeTeam(hub, r2);
        await submit(hub, r2, 'team_admin', {team_id: team2.id});
        const [c2, id2] = await cardFor(hub, r2);
        await deactivate(hub, r2);
        expect(await approve(c2, id2)).toBe(REQUESTER_INACTIVE);
        await expectStillPending(hub, 'team_admin', r2, c2, id2);
        expect((await teamMember(hub, team2.id, r2.id))?.scheme_admin).toBe(false);

        // Nominee left the team, then a nominee was deactivated.
        const r3 = await hub.createUser();
        const n3 = await hub.createUser();
        const n4 = await hub.createUser();
        const team3 = await makeTeam(hub, r3, n3, n4);
        await submitDialog(hub, r3, '/api/v1/team_admin_dialog', 'team_admin_request', team3.id, {members: `${n3.id},${n4.id}`});
        const [c3, id3] = await cardFor(hub, r3);
        await admin.removeFromTeam(team3.id, n3.id);
        expect(await approve(c3, id3)).toBe(`@${n3.username} is no longer a member of the team. Deny this request instead.`);
        await admin.addToTeam(team3.id, n3.id);
        await deactivate(hub, n4);
        expect(await approve(c3, id3)).toBe(`@${n4.username} is deactivated. Deny this request instead.`);
        await expectStillPending(hub, 'team_admin', r3, c3, id3);
        expect((await teamMember(hub, team3.id, n3.id))?.scheme_admin).toBe(false);

        // Team archived, and the type turned off.
        await archiveTeam(hub, team3.id);
        expect(await approve(c3, id3)).toBe('The team has been archived. Deny this request instead.');
        await hub.withPluginConfig({allowteamadminrequests: false}, async () => {
            expect(await approve(c3, id3)).toBe(TYPE_OFF);

            // Deny still closes a request whose type was turned off.
            await admin.doPostAction(c3.id, 'deny');
        });
        await expectResolved(hub, 'team_admin', r3, c3, id3, 'Denied by');
    });
});

test.describe('team member', () => {
    test('REST: a user and a bot added to a team the requester is not on', async ({hub}) => {
        const requester = await hub.createUser();
        const user = await hub.createUser({inTeam: false});
        const admin = await hub.client(ADMIN);
        const bot = await makeBot(hub, uid('bot'));
        const team = await makeTeam(hub);
        const res = await submit(hub, requester, 'team_member', {team_id: team.id, nominees: [`@${user.username}`, bot.username], reason: 'needs access'});
        expect(res).toEqual({message: expect.stringContaining(SUBMITTED.team_member)});

        const [card, id] = await cardFor(hub, requester);
        expect(card.message).toContain('a team member request needs System Admin review');
        expect(cardText(card)).toContain('needs access');
        await hub.findDM(requester.username, SUBMITTED.team_member);
        expect(await hub.kv(`team_member_request_${id}`)).toMatchObject({team_id: team.id, nominee_ids: [user.id, bot.user_id], reason: 'needs access'});
        expect(await pendingIds(hub, 'team_member', requester.id)).toEqual([id]);

        await admin.doPostAction(card.id, 'approve');
        const outcome = `Approved by @${ADMIN} for @${requester.username}. Added @${user.username}, @${bot.username} to **${team.display_name}**.`;
        await expectResolved(hub, 'team_member', requester, card, id, outcome);
        expect(await teamMember(hub, team.id, user.id)).toBeDefined();
        expect(await teamMember(hub, team.id, bot.user_id)).toBeDefined();
        expect(await teamMember(hub, team.id, requester.id)).toBeUndefined();
        await hub.findDM(requester.username, 'Team Member Request Approved');
        await hub.findAudit(`@${ADMIN} added @${user.username}, @${bot.username} to **${team.display_name}** (requested by @${requester.username})`);
    });

    test('slash: denied, nobody is added', async ({hub}) => {
        const requester = await hub.createUser();
        const user = await hub.createUser({inTeam: false});
        expect(await hub.slash(requester.username, `team-member @${user.username} --reason for the project`)).toContain(SUBMITTED.team_member);
        const [card, id] = await cardFor(hub, requester);
        expect(await hub.kv(`team_member_request_${id}`)).toMatchObject({team_id: hub.teamId, reason: 'for the project'});

        await (await hub.client(ADMIN)).doPostAction(card.id, 'deny');
        await expectResolved(hub, 'team_member', requester, card, id, `Denied by @${ADMIN} for @${requester.username}. Team member add for @${user.username} in **Test**.`);
        expect(await teamMember(hub, hub.teamId, user.id)).toBeUndefined();
        await hub.findDM(requester.username, 'Team Member Request Denied');
        await hub.findAudit(`@${ADMIN} denied adding @${user.username} to **Test**`);
    });

    test('refused at submit when team member requests are off', async ({hub}) => {
        const requester = await hub.createUser();
        const user = await hub.createUser({inTeam: false});
        await hub.withPluginConfig({allowteammemberrequests: false}, async () => {
            expect(await submit(hub, requester, 'team_member', {team_id: hub.teamId, nominees: [user.username]})).toEqual({error: DISABLED_MSG.team_member});
            expect(await hub.slash(requester.username, `team-member @${user.username}`)).toBe(DISABLED_MSG.team_member);
            expect(await submitDialog(hub, requester, '/api/v1/team_member_dialog', 'team_member_request', hub.teamId, {members: [user.id]})).toBe(DISABLED_MSG.team_member);
        });
        await expectNoRequest(hub, 'team_member', requester);
        expect(await teamMember(hub, hub.teamId, user.id)).toBeUndefined();
    });

    test('refused at submit: unknown and deactivated users, empty lists, bad teams, long reasons', async ({hub}) => {
        const requester = await hub.createUser();
        const gone = await hub.createUser({inTeam: false});
        await deactivate(hub, gone);
        const nobody = uid('nobody');
        const body = (nominees: string[], extra: Record<string, unknown> = {}) => ({team_id: hub.teamId, nominees, ...extra});

        expect(await submit(hub, requester, 'team_member', body([nobody]))).toEqual({error: `unknown user: ${nobody}`});
        expect(await hub.slash(requester.username, `team-member @${nobody}`)).toBe(`unknown user: ${nobody}`);
        expect(await submit(hub, requester, 'team_member', body([gone.username]))).toEqual({error: `@${gone.username} is deactivated and can't be made a team member`});
        expect(await submit(hub, requester, 'team_member', body([]))).toEqual({error: 'pick at least one person to add to the team'});
        expect(await hub.slash(requester.username, 'team-member --reason x')).toContain('Could not read the request: name at least one user.');
        expect(await submit(hub, requester, 'team_member', body(Array.from({length: 101}, (_, i) => `u${i}`)))).toEqual({error: 'too many users: at most 100 per list'});
        expect(await submit(hub, requester, 'team_member', {team_id: '', nominees: [ADMIN]})).toEqual({error: 'team_id is required'});
        expect(await submit(hub, requester, 'team_member', {team_id: 'c'.repeat(26), nominees: [ADMIN]})).toEqual({error: 'could not find that team'});
        expect(await submit(hub, requester, 'team_member', body([ADMIN], {reason: 'r'.repeat(501)}))).toEqual({error: 'reason must be 500 characters or fewer'});
        expect(await hub.slash(requester.username, `team-member @${ADMIN} --reason`)).toContain('`--reason` needs some text');
        expect(await submitDialog(hub, requester, '/api/v1/team_member_dialog', 'team_member_request', hub.teamId, {members: ['not-an-id']})).toBe('people to add must be picked from the list');
        await expectNoRequest(hub, 'team_member', requester);
    });

    test('refused at submit: a second pending request for the same people', async ({hub}) => {
        const requester = await hub.createUser();
        const user = await hub.createUser({inTeam: false});
        await submit(hub, requester, 'team_member', {team_id: hub.teamId, nominees: [user.username]});
        expect(await hub.slash(requester.username, `team-member @${user.username}`)).toBe('you already have a pending request to add these people to this team');
        expect(await recordsBy(hub, 'team_member', requester.id)).toHaveLength(1);
    });

    test('refused at submit when every nominee is already on the team', async ({hub}) => {
        test.fail(true, 'BUG: submitTeamMemberRequest accepts users already on the team; approval then reports them as "Added"');
        const requester = await hub.createUser();
        const member = await hub.createUser();
        expect(await hub.slash(requester.username, `team-member @${member.username}`)).not.toContain(SUBMITTED.team_member);
        await expectNoRequest(hub, 'team_member', requester);
    });

    test('refused at approval: non System Admins, archived team, deactivated requester or nominee', async ({hub}) => {
        const admin = await hub.client(ADMIN);
        const ta = await hub.createUser({teamAdmin: true});
        const requester = await hub.createUser();
        const user = await hub.createUser({inTeam: false});
        const other = await hub.createUser({inTeam: false});
        const team = await makeTeam(hub);
        await submit(hub, requester, 'team_member', {team_id: team.id, nominees: [user.username, other.username]});
        const [card, id] = await cardFor(hub, requester);
        const approve = async (as = ADMIN) => (await decide(hub, as, 'team_member', card, id, true)).ephemeral_text;

        // Only System Admins, whatever the Team Admin settings say.
        await hub.withPluginConfig({allowapprovalteamadminsforteamrequests: true, allowteamadminapprovers: true}, async () => {
            expect(await approve(ta.username)).toBe('Only System Admins can approve or deny team member requests.');
            expect((await decide(hub, requester.username, 'team_member', card, id, false)).ephemeral_text).toBe('Only System Admins can approve or deny team member requests.');
        });
        await hub.withPluginConfig({allowteammemberrequests: false}, async () => {
            expect(await approve()).toBe(TYPE_OFF);
        });

        await deactivate(hub, other);
        expect(await approve()).toBe(`@${other.username} is deactivated. Deny this request instead.`);
        await admin.updateUserActive(other.id, true);
        await deactivate(hub, requester);
        expect(await approve()).toBe(REQUESTER_INACTIVE);
        await admin.updateUserActive(requester.id, true);
        await archiveTeam(hub, team.id);
        expect(await approve()).toBe('The team has been archived. Deny this request instead.');
        await expectStillPending(hub, 'team_member', requester, card, id);
        expect(await teamMember(hub, team.id, user.id)).toBeUndefined();

        await admin.doPostAction(card.id, 'deny');
        await expectResolved(hub, 'team_member', requester, card, id, 'Denied by');
    });
});

test.describe('team guest', () => {
    test('off by default: refused on every entry point', async ({hub}) => {
        const requester = await hub.createUser();
        const townSquare = await hub.channelId('town-square');
        expect(await submit(hub, requester, 'team_guest', {team_id: hub.teamId, email: `${uid()}@example.com`, channel_ids: [townSquare]})).toEqual({error: DISABLED_MSG.team_guest});
        expect(await hub.slash(requester.username, `team-guest ${uid()}@example.com --channels ~town-square`)).toBe(DISABLED_MSG.team_guest);
        expect(await hub.slash(requester.username, 'team-guest')).toBe(DISABLED_MSG.team_guest);
        expect(await submitDialog(hub, requester, '/api/v1/team_guest_dialog', 'team_guest_request', hub.teamId, {email: `${uid()}@example.com`, guest_channels: [townSquare]})).toBe(DISABLED_MSG.team_guest);
        await expectNoRequest(hub, 'team_guest', requester);
    });

    test('REST: approval sends a guest invitation for the requested channels', async ({hub}) => {
        const requester = await hub.createUser();
        const email = `${uid('guest')}@example.com`;
        const townSquare = await hub.channelId('town-square');
        await hub.withPluginConfig({allowteamguestrequests: true}, () => withGuestAccounts(hub, async () => {
            const res = await submit(hub, requester, 'team_guest', {team_id: hub.teamId, email: email.toUpperCase(), channel_ids: [townSquare], reason: 'contractor'});
            expect(res).toEqual({message: expect.stringContaining(SUBMITTED.team_guest)});
            const [card, id] = await cardFor(hub, requester);
            expect(card.message).toContain('a guest invite request needs System Admin review');
            expect(cardText(card)).toContain(email);
            await hub.findDM(requester.username, SUBMITTED.team_guest);
            expect(await hub.kv(`team_guest_request_${id}`)).toMatchObject({email, channel_ids: [townSquare], reason: 'contractor'});

            await (await hub.client(ADMIN)).doPostAction(card.id, 'approve');
            await expectResolved(hub, 'team_guest', requester, card, id, `Approved by @${ADMIN} for @${requester.username}. Guest invitation emailed to ${email} for **Test**.`);
            await hub.findDM(requester.username, 'Guest Invite Request Approved');
            const audit = await hub.findAudit(`(requested by @${requester.username})`);
            expect(cardText(audit)).toContain(`@${ADMIN} guest invite for **Test** email "${email}"`);

            // Mattermost keeps the invitation as a token naming the email,
            // team and channels (no mail server, so nothing is delivered).
            const token = await hub.mm.psql(`SELECT extra FROM tokens WHERE type = 'guest_invitation' AND extra LIKE '%${email}%'`);
            expect(JSON.parse(token)).toMatchObject({email, teamId: hub.teamId, channels: townSquare});
        }));
    });

    test('slash: denied, no invitation is sent', async ({hub}) => {
        const requester = await hub.createUser();
        const email = `${uid('guest')}@example.com`;
        await hub.withPluginConfig({allowteamguestrequests: true}, async () => {
            expect(await hub.slash(requester.username, `team-guest ${email} --channels ~town-square --reason a visit`)).toContain(SUBMITTED.team_guest);
            const [card, id] = await cardFor(hub, requester);
            expect(await hub.kv(`team_guest_request_${id}`)).toMatchObject({email, reason: 'a visit'});
            await (await hub.client(ADMIN)).doPostAction(card.id, 'deny');
            await expectResolved(hub, 'team_guest', requester, card, id, `Denied by @${ADMIN} for @${requester.username}. Guest invite for **Test**.`);
        });
        await hub.findDM(requester.username, 'Guest Invite Request Denied');
        await hub.findAudit(`@${ADMIN} denied guest invite for **Test**`);
        expect(await hub.mm.psql(`SELECT count(*) FROM tokens WHERE extra LIKE '%${email}%'`)).toBe('0');
    });

    test('refused at submit: bad emails, channels and teams, outsiders, long reasons, duplicates', async ({hub}) => {
        const requester = await hub.createUser();
        const admin = await hub.client(ADMIN);
        const townSquare = await hub.channelId('town-square');
        const notMine = await admin.createChannel({team_id: hub.teamId, name: uid('c'), display_name: 'Not mine', type: 'O'} as Parameters<typeof admin.createChannel>[0]);
        const archived = await (await hub.client(requester.username)).createChannel({team_id: hub.teamId, name: uid('c'), display_name: 'Archived', type: 'O'} as Parameters<typeof admin.createChannel>[0]);
        await admin.deleteChannel(archived.id);
        const otherTeam = await makeTeam(hub, requester);
        const elsewhere = await admin.getChannelByName(otherTeam.id, 'town-square');
        const email = `${uid('guest')}@example.com`;
        const rest = (extra: Record<string, unknown>) => submit(hub, requester, 'team_guest', {team_id: hub.teamId, email, channel_ids: [townSquare], ...extra});

        await hub.withPluginConfig({allowteamguestrequests: true}, async () => {
            expect(await rest({email: 'not-an-email'})).toEqual({error: 'a valid guest email is required'});
            expect(await rest({email: `${'a'.repeat(120)}@example.com`})).toEqual({error: 'a valid guest email is required'});
            expect(await rest({channel_ids: []})).toEqual({error: 'pick at least one channel the guest should access'});
            expect(await rest({channel_ids: ['bad']})).toEqual({error: 'guest channels must be picked from the list'});
            expect(await rest({channel_ids: [elsewhere.id]})).toEqual({error: 'all guest channels must belong to the selected team'});
            expect(await rest({channel_ids: [notMine.id]})).toEqual({error: 'you can only invite guests to channels you\'re a member of'});
            expect(await rest({channel_ids: [archived.id]})).toEqual({error: `~${archived.name} is archived`});
            expect(await rest({reason: 'r'.repeat(501)})).toEqual({error: 'reason must be 500 characters or fewer'});
            expect(await rest({team_id: 'd'.repeat(26)})).toEqual({error: 'could not find that team'});
            expect(await rest({team_id: ''})).toEqual({error: 'team_id is required'});
            await admin.removeFromTeam(otherTeam.id, requester.id);
            expect(await rest({team_id: otherTeam.id, channel_ids: [elsewhere.id]})).toEqual({error: 'you must be a member of the team to request a guest invite'});

            expect(await hub.slash(requester.username, `team-guest ${email}`)).toContain('name at least one channel with `--channels`');
            expect(await hub.slash(requester.username, `team-guest ${email} other@example.com --channels ~town-square`)).toContain('give one guest email');
            expect(await hub.slash(requester.username, `team-guest ${email} --channels ~${uid('nochannel')}`)).toMatch(/^unknown channel: ~nochannel/);
            await expectNoRequest(hub, 'team_guest', requester);

            // The same email again, in any case, is a duplicate.
            expect((await rest({})).message).toContain(SUBMITTED.team_guest);
            expect(await rest({email: email.toUpperCase()})).toEqual({error: `you already have a pending request to invite ${email} to this team`});
            expect(await recordsBy(hub, 'team_guest', requester.id)).toHaveLength(1);
        });
    });

    test('refused at approval: guest accounts off, non System Admins, channel and team re-checks', async ({hub}) => {
        const admin = await hub.client(ADMIN);
        const requester = await hub.createUser();
        const ta = await hub.createUser({teamAdmin: true});
        const channel = await (await hub.client(requester.username)).createChannel({team_id: hub.teamId, name: uid('c'), display_name: 'Guest room', type: 'P'} as Parameters<typeof admin.createChannel>[0]);
        const email = `${uid('guest')}@example.com`;
        const noInvite = async () => expect(await hub.mm.psql(`SELECT count(*) FROM tokens WHERE extra LIKE '%${email}%'`)).toBe('0');

        await hub.withPluginConfig({allowteamguestrequests: true}, async () => {
            await submit(hub, requester, 'team_guest', {team_id: hub.teamId, email, channel_ids: [channel.id]});
            const [card, id] = await cardFor(hub, requester);
            const approve = async (as = ADMIN) => (await decide(hub, as, 'team_guest', card, id, true)).ephemeral_text;

            // Guest accounts are off on the server: the invite fails and
            // the request is put back.
            expect(await approve()).toMatch(/^guest email invite failed \(.+\)\. Guest Accounts may be disabled$/);
            await expectStillPending(hub, 'team_guest', requester, card, id);
            await noInvite();

            await hub.withPluginConfig({allowapprovalteamadminsforteamrequests: true}, async () => {
                expect(await approve(ta.username)).toBe('Only System Admins can approve or deny guest invite requests.');
            });

            await admin.removeFromChannel(requester.id, channel.id);
            expect(await approve()).toBe('The requester is no longer a member of the channel. Deny this request instead.');
            await admin.addToChannel(requester.id, channel.id);
            await admin.deleteChannel(channel.id);
            expect(await approve()).toBe(`~${channel.name} has been archived. Deny this request instead.`);
            await deactivate(hub, requester);
            expect(await approve()).toBe(REQUESTER_INACTIVE);
            await expectStillPending(hub, 'team_guest', requester, card, id);
            await noInvite();

            await admin.doPostAction(card.id, 'deny');
            await expectResolved(hub, 'team_guest', requester, card, id, 'Denied by');
        });
    });

    test('refused at approval when the team was archived', async ({hub}) => {
        const requester = await hub.createUser();
        const team = await makeTeam(hub, requester);
        const townSquare = await (await hub.client(ADMIN)).getChannelByName(team.id, 'town-square');
        await hub.withPluginConfig({allowteamguestrequests: true}, async () => {
            await submit(hub, requester, 'team_guest', {team_id: team.id, email: `${uid('guest')}@example.com`, channel_ids: [townSquare.id]});
            const [card, id] = await cardFor(hub, requester);
            await archiveTeam(hub, team.id);
            expect((await decide(hub, ADMIN, 'team_guest', card, id, true)).ephemeral_text).toBe('The team has been archived. Deny this request instead.');
            await expectStillPending(hub, 'team_guest', requester, card, id);
            await hub.withPluginConfig({allowteamguestrequests: false}, async () => {
                expect((await decide(hub, ADMIN, 'team_guest', card, id, true)).ephemeral_text).toBe(TYPE_OFF);
            });
        });
    });
});

test.describe('team menu', () => {
    const openTeamMenu = async (page: import('@playwright/test').Page) => {
        await page.locator('#sidebarTeamMenuButton').click();
        await page.getByRole('menuitem', {name: /Leave team|Learn about teams/}).first().waitFor();
    };

    // Clicks the open menu's invisible backdrop away from the form.
    const closeStuckMenu = async (page: import('@playwright/test').Page) => {
        const backdrop = page.locator('#backdropForMenuComponent');
        if (await backdrop.count() > 0) {
            await backdrop.click({position: {x: 5, y: 5}, force: true});
            await expect(backdrop).toHaveCount(0);
        }
    };

    test('a member gets Request a Team and Request Team Admin, which open the plugin forms', async ({hub, page}) => {
        const requester = await hub.createUser();
        await login(page, hub, requester.username);
        await openChannel(page, hub, 'town-square');

        // The forms are found by CSS, not by role, and the menu is closed by
        // hand: it stays open over them (see the next test).
        await openTeamMenu(page);
        await expect(page.getByText('Create a team', {exact: true})).toHaveCount(0);
        await page.getByText('Request a Team', {exact: true}).click();
        const teamForm = page.locator('[role="dialog"]').filter({hasText: 'Request a New Team'});
        await expect(teamForm).toBeVisible();
        await closeStuckMenu(page);
        await teamForm.locator('button', {hasText: 'Cancel'}).click();
        await expect(teamForm).toBeHidden();

        await openTeamMenu(page);
        await page.getByText('Request Team Admin', {exact: true}).click();
        const adminForm = page.locator('[role="dialog"]').filter({hasText: 'Request Team Admin'});
        await expect(adminForm).toBeVisible();
        await closeStuckMenu(page);
        await adminForm.locator('button', {hasText: 'Submit request'}).click();
        const [card, id] = await cardFor(hub, requester);
        expect(await hub.kv(`team_admin_request_${id}`)).toMatchObject({team_id: hub.teamId, nominee_ids: [requester.id]});
        expect(actionIds(card)).toEqual(['approve', 'deny']);
    });

    test('choosing a plugin item closes the team menu', async ({hub, page}) => {
        test.fail(true, 'BUG: teamMenuOverride dispatches Escape on document, which the MUI menu ignores; its backdrop stays over the form and swallows the first click');
        const requester = await hub.createUser();
        await login(page, hub, requester.username);
        await openChannel(page, hub, 'town-square');
        await openTeamMenu(page);
        await page.getByText('Request a Team', {exact: true}).click();
        await expect(page.locator('[role="dialog"]').filter({hasText: 'Request a New Team'})).toBeVisible();
        await expect(page.locator('#backdropForMenuComponent')).toHaveCount(0, {timeout: 3000});
    });

    test('System Admins keep the native Create a team and get no Request Team Admin', async ({hub, page}) => {
        const sysadmin = await hub.createUser({systemAdmin: true});
        await login(page, hub, sysadmin.username);
        await openChannel(page, hub, 'town-square');
        await openTeamMenu(page);
        await expect(page.getByText('Create a team', {exact: true})).toBeVisible();
        await expect(page.getByText('Request a Team', {exact: true})).toHaveCount(0);
        await expect(page.getByText('Request Team Admin', {exact: true})).toHaveCount(0);
    });

    test('slash team-admin form: a nominee picked in the dialog is submitted', async ({hub, page}) => {
        test.fail(true, 'BUG: the multi-select arrives as an array but handleTeamAdminDialogSubmit reads it with submissionString, so every submit says "pick at least one person"');
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        await login(page, hub, requester.username);
        await openChannel(page, hub, 'town-square');
        await page.getByTestId('post_textbox').fill('/mattermost-request-hub team-admin');
        await page.getByText('Request admin-gated access via Request Hub').or(page.getByText('Request Team Admin role in the current team')).first().waitFor();
        await page.getByTestId('SendMessageButton').click();

        const dialog = page.getByRole('dialog').filter({hasText: 'Request Team Admin Promotion'});
        await expect(dialog).toBeVisible();
        await dialog.getByRole('combobox').first().click();
        await page.keyboard.type(nominee.username);
        await page.getByText(`@${nominee.username}`).first().click();
        await dialog.getByRole('button', {name: 'Submit request'}).click();
        await expect(dialog.getByText('pick at least one person to make a Team Admin')).toHaveCount(0);
        await expect(dialog).toBeHidden();

        const [, id] = await cardFor(hub, requester);
        expect(await hub.kv(`team_admin_request_${id}`)).toMatchObject({team_id: hub.teamId, nominee_ids: [nominee.id]});
    });
});

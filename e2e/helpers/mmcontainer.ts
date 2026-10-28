// Mattermost + Postgres in Docker via testcontainers, modelled on
// mattermost-plugin-agents' e2e/helpers/mmcontainer.ts.
import fs from 'fs';
import path from 'path';

import {Client4} from '@mattermost/client';
import {PostgreSqlContainer, type StartedPostgreSqlContainer} from '@testcontainers/postgresql';
import {GenericContainer, Network, Wait, type StartedNetwork, type StartedTestContainer} from 'testcontainers';

// Same default as docker-compose.dev.yml. CI can pin another tag through
// the MM_IMAGE repository variable.
const defaultMattermostImage = 'mattermost/mattermost-enterprise-edition:latest';
export const POSTGRES_IMAGE = 'postgres:14-alpine';

// The enterprise image is published for linux/amd64 only; Apple Silicon
// runs it under Rosetta. Same override as docker-compose.dev.yml.
const platform = process.env.MATTERMOST_PLATFORM || 'linux/amd64';

export function mattermostImage(): string {
    return process.env.MM_IMAGE || defaultMattermostImage;
}

export const LOG_DIR = path.join(__dirname, '..', 'logs');

export default class MattermostContainer {
    container!: StartedTestContainer;
    pgContainer!: StartedPostgreSqlContainer;
    network!: StartedNetwork;
    logFile = '';
    private envs: Record<string, string> = {
        MM_SQLSETTINGS_DRIVERNAME: 'postgres',
        MM_SQLSETTINGS_DATASOURCE: 'postgres://user:pass@db:5432/mattermost_test?sslmode=disable&connect_timeout=10',
        MM_SERVICESETTINGS_ENABLELOCALMODE: 'true',
        MM_SERVICESETTINGS_ENABLEONBOARDINGFLOW: 'false',
        MM_PLUGINSETTINGS_ENABLEUPLOADS: 'true',
        MM_PLUGINSETTINGS_AUTOMATICPREPACKAGEDPLUGINS: 'false',
        MM_LOGSETTINGS_CONSOLELEVEL: 'DEBUG',
        MM_LOGSETTINGS_CONSOLEJSON: 'true',
        MM_EMAILSETTINGS_SENDEMAILNOTIFICATIONS: 'false',
        MM_ANNOUNCEMENTSETTINGS_ADMINNOTICESENABLED: 'false',
        MM_ANNOUNCEMENTSETTINGS_USERNOTICESENABLED: 'false',
    };

    url(): string {
        return `http://${this.container.getHost()}:${this.container.getMappedPort(8065)}`;
    }

    getClient = async (username: string, password: string): Promise<Client4> => {
        const client = new Client4();
        client.setUrl(this.url());
        await client.login(username, password);
        return client;
    };

    // container.exec resolves even when the command exits non-zero, so check
    // the exit code; otherwise a failed setup step surfaces later as an
    // unrelated error.
    mmctl = async (...args: string[]): Promise<string> => {
        const {output, exitCode} = await this.container.exec(['mmctl', '--local', ...args]);
        if (exitCode !== 0) {
            throw new Error(`mmctl ${args.join(' ')} exited with code ${exitCode}: ${output.trim()}`);
        }
        return output;
    };

    // "Server is listening" is logged before the local-mode socket accepts
    // mmctl connections.
    private waitForLocalMode = async (timeoutMs = 120_000) => {
        const deadline = Date.now() + timeoutMs;
        let lastOutput = '';
        while (Date.now() < deadline) {
            const {output, exitCode} = await this.container.exec(['mmctl', '--local', 'system', 'version']);
            if (exitCode === 0) {
                return;
            }
            lastOutput = output.trim();
            await new Promise((resolve) => setTimeout(resolve, 500));
        }
        throw new Error(`mmctl local mode not ready after ${timeoutMs}ms: ${lastOutput}`);
    };

    // Server log lines written since the container started, one JSON object
    // per line (MM_LOGSETTINGS_CONSOLEJSON).
    logLines = (): string[] => fs.readFileSync(this.logFile, 'utf8').split('\n').filter(Boolean);

    start = async (name: string): Promise<MattermostContainer> => {
        const image = mattermostImage();
        console.log(`Starting ${image} for ${name}`);

        fs.mkdirSync(LOG_DIR, {recursive: true});
        this.logFile = path.join(LOG_DIR, `${name}.log`);
        const logStream = fs.createWriteStream(this.logFile, {flags: 'w'});

        this.network = await new Network().start();
        this.pgContainer = await new PostgreSqlContainer(POSTGRES_IMAGE).
            withDatabase('mattermost_test').
            withUsername('user').
            withPassword('pass').
            withNetwork(this.network).
            withNetworkAliases('db').
            start();

        this.container = await new GenericContainer(image).
            withPlatform(platform).
            withEnvironment(this.envs).
            withExposedPorts(8065).
            withNetwork(this.network).
            withNetworkAliases('mattermost').
            withWaitStrategy(Wait.forLogMessage('Server is listening on').withStartupTimeout(240_000)).
            withLogConsumer((stream) => stream.on('data', (line) => logStream.write(line))).
            start();

        await this.waitForLocalMode();
        await this.mmctl('config', 'set', 'ServiceSettings.SiteURL', this.url());
        return this;
    };

    stop = async () => {
        await this.container?.stop();
        await this.pgContainer?.stop();
        await this.network?.stop();
    };
}

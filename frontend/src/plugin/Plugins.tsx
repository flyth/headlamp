import { useEffect } from 'react';
import { useDispatch } from 'react-redux';
import helpers from '../helpers';
import { setPluginsLoadState, UI_INITIALIZE_PLUGIN_VIEWS } from '../redux/actions/actions';
import { initializePlugins } from './index';

/**
 * For discovering and executing plugins.
 *
 * Compared to loading plugins in script tags, doing it this way has some benefits:
 *
 * 1) We can more easily catch certain types of errors in execution. With a
 *   script tag we can not catch errors in the running scripts.
 * 2) We can load the scripts into a context other than global/window. This
 *   means that plugins do not pollute the global namespace.
 */
export default function Plugins() {
  const dispatch = useDispatch();

  // only run on first load
  useEffect(() => {
    dispatch({ type: UI_INITIALIZE_PLUGIN_VIEWS });

    /**
     * Get the list of plugins,
     *   download all the plugin source,
     *   download all the plugin package.json files,
     *   execute the plugins,
     *   .initialize() plugins that register (not all do).
     */
    async function fetchAndExecute() {
      const pluginPaths = (await fetch(`${helpers.getAppUrl()}plugins`).then(resp =>
        resp.json()
      )) as string[];

      const sourcesPromise = Promise.all(
        pluginPaths.map(path =>
          fetch(`${helpers.getAppUrl()}${path}/main.js`).then(resp => resp.text())
        )
      );

      // fetch the packages. But if there is a problem,
      const packageInfosPromise = await Promise.all<{}[]>(
        pluginPaths.map(path =>
          fetch(`${helpers.getAppUrl()}${path}/package.json`).then(resp => {
            if (!resp.ok) {
              if (resp.status !== 404) {
                return Promise.reject(resp);
              }
              {
                console.warn(
                  'Missing package.json. ' +
                    `Please upgrade the plugin ${path}` +
                    ' by running "headlamp-plugin extract" again.' +
                    ' Please use headlamp-plugin >= 0.6.0'
                );
                return {
                  name: path.split('/').slice(-1)[0],
                  version: '0.0.0',
                  author: 'unknown',
                  description: '',
                };
              }
            }
            return resp.json();
          })
        )
      );

      const sources = await sourcesPromise;
      const packageInfos = await packageInfosPromise;

      /**
       * This can be used to filter out which of the plugins we should execute.
       *
       * @param sources array of source to execute
       * @param packageInfos array of package.json contents
       * @returns array of source to execute
       */
      // eslint-disable-next-line no-unused-vars
      function getSourcesToExecute(sources: string[], packageInfos: {}[]) {
        //@todo: filter out plugins which are not enabled.
        //@todo: packageInfos and some info from plugin-settings will be used for this.
        return sources;
      }

      getSourcesToExecute(sources, packageInfos).forEach((source, index) => {
        // Execute plugins inside a context (not in global/window)
        (function (str: string) {
          try {
            const result = eval(str);
            return result;
          } catch (e) {
            // We just continue if there is an error.
            console.error(`Plugin execution error in ${pluginPaths[index]}:`, e);
          }
        }.call({}, source));
      });
      await initializePlugins();
    }

    fetchAndExecute()
      .finally(() => {
        dispatch(setPluginsLoadState(true));
        // Warn the app (if we're in app mode).
        window.desktopApi?.send('pluginsLoaded');
      })
      .catch(console.error);
  }, []);
  return null;
}
